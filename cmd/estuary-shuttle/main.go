package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/urfave/cli/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/net/websocket"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
	"gorm.io/gorm"

	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/lotus/api"
	lcli "github.com/filecoin-project/lotus/cli"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	chunker "github.com/ipfs/go-ipfs-chunker"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-unixfs/importer"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/whyrusleeping/estuary/drpc"
	"github.com/whyrusleeping/estuary/filclient"
	node "github.com/whyrusleeping/estuary/node"
	"github.com/whyrusleeping/estuary/pinner"
	"github.com/whyrusleeping/estuary/stagingbs"
	"github.com/whyrusleeping/estuary/util"
	"github.com/whyrusleeping/memo"
)

var Tracer = otel.Tracer("shuttle")

var log = logging.Logger("shuttle")

func init() {
	if os.Getenv("FULLNODE_API_INFO") == "" {
		os.Setenv("FULLNODE_API_INFO", "wss://api.chain.love")
	}
}

func main() {
	logging.SetLogLevel("dt-impl", "debug")
	logging.SetLogLevel("shuttle", "debug")
	logging.SetLogLevel("paych", "debug")
	logging.SetLogLevel("filclient", "debug")
	logging.SetLogLevel("dt_graphsync", "debug")
	logging.SetLogLevel("dt-chanmon", "debug")
	logging.SetLogLevel("markets", "debug")
	logging.SetLogLevel("data_transfer_network", "debug")
	logging.SetLogLevel("rpc", "info")
	logging.SetLogLevel("bs-wal", "info")

	app := cli.NewApp()
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:  "repo",
			Value: "~/.lotus",
		},
		&cli.StringFlag{
			Name:    "database",
			Value:   "sqlite=estuary-shuttle.db",
			EnvVars: []string{"ESTUARY_SHUTTLE_DATABASE"},
		},
		&cli.StringFlag{
			Name: "blockstore",
		},
		&cli.StringFlag{
			Name:  "write-log",
			Usage: "enable write log blockstore in specified directory",
		},
		&cli.StringFlag{
			Name:    "apilisten",
			Usage:   "address for the api server to listen on",
			Value:   ":3005",
			EnvVars: []string{"ESTUARY_SHUTTLE_API_LISTEN"},
		},
		&cli.StringFlag{
			Name:    "datadir",
			Usage:   "directory to store data in",
			Value:   ".",
			EnvVars: []string{"ESTUARY_SHUTTLE_DATADIR"},
		},
		&cli.StringFlag{
			Name:  "estuary-api",
			Usage: "api endpoint for master estuary node",
			Value: "api.estuary.tech",
		},
		&cli.StringFlag{
			Name:     "auth-token",
			Usage:    "auth token for connecting to estuary",
			Required: true,
		},
		&cli.StringFlag{
			Name:     "handle",
			Usage:    "estuary shuttle handle to use",
			Required: true,
		},
		&cli.StringFlag{
			Name:  "host",
			Usage: "url that this node is publicly dialable at",
		},
		&cli.BoolFlag{
			Name: "logging",
		},
	}

	app.Action = func(cctx *cli.Context) error {
		ddir := cctx.String("datadir")

		bsdir := cctx.String("blockstore")
		if bsdir == "" {
			bsdir = filepath.Join(ddir, "blocks")
		} else if bsdir[0] != '/' {
			bsdir = filepath.Join(ddir, bsdir)

		}

		wlog := cctx.String("write-log")
		if wlog != "" && wlog[0] != '/' {
			wlog = filepath.Join(ddir, wlog)
		}

		cfg := &node.Config{
			ListenAddrs: []string{
				"/ip4/0.0.0.0/tcp/6745",
			},
			Blockstore:    bsdir,
			WriteLog:      wlog,
			Libp2pKeyFile: filepath.Join(ddir, "peer.key"),
			Datastore:     filepath.Join(ddir, "leveldb"),
			WalletDir:     filepath.Join(ddir, "wallet"),
		}

		api, closer, err := lcli.GetGatewayAPI(cctx)
		if err != nil {
			return err
		}

		defer closer()

		nd, err := node.Setup(context.TODO(), cfg)
		if err != nil {
			return err
		}

		defaddr, err := nd.Wallet.GetDefault()
		if err != nil {
			return err
		}

		filc, err := filclient.NewClient(nd.Host, api, nd.Wallet, defaddr, nd.Blockstore, nd.Datastore, ddir)
		if err != nil {
			return err
		}

		db, err := setupDatabase(cctx.String("database"))
		if err != nil {
			return err
		}

		commpMemo := memo.NewMemoizer(func(ctx context.Context, k string) (interface{}, error) {
			c, err := cid.Decode(k)
			if err != nil {
				return nil, err
			}

			commpcid, size, err := filclient.GeneratePieceCommitment(ctx, c, nd.Blockstore)
			if err != nil {
				return nil, err
			}

			res := &commpResult{
				CommP: commpcid,
				Size:  size,
			}

			return res, nil
		})

		sbm, err := stagingbs.NewStagingBSMgr(filepath.Join(ddir, "staging"))
		if err != nil {
			return err
		}

		d := &Shuttle{
			Node:       nd,
			Api:        api,
			DB:         db,
			Filc:       filc,
			StagingMgr: sbm,

			commpMemo: commpMemo,

			trackingChannels: make(map[string]*chanTrack),

			outgoing: make(chan *drpc.Message),

			hostname:      cctx.String("host"),
			estuaryHost:   cctx.String("estuary-api"),
			shuttleHandle: cctx.String("handle"),
			shuttleToken:  cctx.String("auth-token"),
		}
		d.PinMgr = pinner.NewPinManager(d.doPinning, d.onPinStatusUpdate)

		go d.PinMgr.Run(100)

		if err := d.refreshPinQueue(); err != nil {
			log.Errorf("failed to refresh pin queue: %s", err)
		}

		d.Filc.SubscribeToDataTransferEvents(func(event datatransfer.Event, st datatransfer.ChannelState) {
			chid := st.ChannelID().String()
			d.tcLk.Lock()
			defer d.tcLk.Unlock()
			trk, ok := d.trackingChannels[chid]
			if !ok {
				return
			}

			if trk.last == nil || trk.last.Status != st.Status() {
				cst := filclient.ChannelStateConv(st)
				trk.last = cst

				log.Infof("event(%d) message: %s", event.Code, event.Message)
				go d.sendTransferStatusUpdate(context.TODO(), &drpc.TransferStatus{
					Chanid:   chid,
					DealDBID: trk.dbid,
					State:    cst,
				})
			}
		})

		go func() {
			if err := http.ListenAndServe("127.0.0.1:3105", nil); err != nil {
				log.Errorf("failed to start http server for pprof endpoints: %s", err)
			}
		}()

		go func() {
			if err := d.RunRpcConnection(); err != nil {
				log.Errorf("failed to run rpc connection: %s", err)
			}
		}()

		go func() {
			upd, err := d.getUpdatePacket()
			if err != nil {
				log.Errorf("failed to get update packet: %s", err)
			}

			if err := d.sendRpcMessage(context.TODO(), &drpc.Message{
				Op: drpc.OP_ShuttleUpdate,
				Params: drpc.MsgParams{
					ShuttleUpdate: upd,
				},
			}); err != nil {
				log.Errorf("failed to send shuttle update: %s", err)
			}
			for range time.Tick(time.Minute) {
				upd, err := d.getUpdatePacket()
				if err != nil {
					log.Errorf("failed to get update packet: %s", err)
				}

				if err := d.sendRpcMessage(context.TODO(), &drpc.Message{
					Op: drpc.OP_ShuttleUpdate,
					Params: drpc.MsgParams{
						ShuttleUpdate: upd,
					},
				}); err != nil {
					log.Errorf("failed to send shuttle update: %s", err)
				}
			}

		}()

		return d.ServeAPI(cctx.String("apilisten"), cctx.Bool("logging"))
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

type Shuttle struct {
	Node       *node.Node
	Api        api.Gateway
	DB         *gorm.DB
	PinMgr     *pinner.PinManager
	Filc       *filclient.FilClient
	StagingMgr *stagingbs.StagingBSMgr

	tcLk             sync.Mutex
	trackingChannels map[string]*chanTrack

	addPinLk sync.Mutex

	outgoing chan *drpc.Message

	hostname      string
	estuaryHost   string
	shuttleHandle string
	shuttleToken  string

	commpMemo *memo.Memoizer
}

type chanTrack struct {
	dbid uint
	last *filclient.ChannelState
}

func (d *Shuttle) RunRpcConnection() error {
	for {
		conn, err := d.dialConn()
		if err != nil {
			log.Errorf("failed to dial estuary rpc endpoint: %s", err)
			time.Sleep(time.Second * 10)
			continue
		}

		if err := d.runRpc(conn); err != nil {
			log.Errorf("rpc routine exited with an error: %s", err)
			time.Sleep(time.Second * 10)
			continue
		}

		log.Warnf("rpc routine exited with no error, reconnecting...")
		time.Sleep(time.Second)
	}
}

func (d *Shuttle) runRpc(conn *websocket.Conn) error {
	log.Infof("connecting to primary estuary node")
	defer conn.Close()

	readDone := make(chan struct{})

	// Send hello message
	hello, err := d.getHelloMessage()
	if err != nil {
		return err
	}

	if err := websocket.JSON.Send(conn, hello); err != nil {
		return err
	}

	go func() {
		defer close(readDone)

		for {
			var cmd drpc.Command
			if err := websocket.JSON.Receive(conn, &cmd); err != nil {
				log.Errorf("failed to read command from websocket: %s", err)
				return
			}

			go func(cmd *drpc.Command) {
				if err := d.handleRpcCmd(cmd); err != nil {
					log.Errorf("failed to handle rpc command: %s", err)
				}
			}(&cmd)
		}
	}()

	for {
		select {
		case <-readDone:
			return fmt.Errorf("read routine exited, assuming socket is closed")
		case msg := <-d.outgoing:
			conn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err := websocket.JSON.Send(conn, msg); err != nil {
				log.Errorf("failed to send message: %s", err)
			}
			conn.SetWriteDeadline(time.Time{})
		}
	}
}

func (d *Shuttle) getHelloMessage() (*drpc.Hello, error) {
	addr, err := d.Node.Wallet.GetDefault()
	if err != nil {
		return nil, err
	}

	log.Infow("sending hello", "hostname", d.hostname, "address", addr, "pid", d.Node.Host.ID())
	return &drpc.Hello{
		Host:    d.hostname,
		PeerID:  d.Node.Host.ID().Pretty(),
		Address: addr,
		AddrInfo: peer.AddrInfo{
			ID:    d.Node.Host.ID(),
			Addrs: d.Node.Host.Addrs(),
		},
	}, nil
}

func (d *Shuttle) dialConn() (*websocket.Conn, error) {
	cfg, err := websocket.NewConfig("wss://"+d.estuaryHost+"/shuttle/conn", "http://localhost")
	if err != nil {
		return nil, err
	}

	cfg.Header.Set("Authorization", "Bearer "+d.shuttleToken)

	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

type User struct {
	ID       uint
	Username string
	Perms    int

	AuthToken       string `json:"-"` // this struct shouldnt ever be serialized, but just in case...
	StorageDisabled bool
}

func (d *Shuttle) checkTokenAuth(token string) (*User, error) {
	req, err := http.NewRequest("GET", "https://"+d.estuaryHost+"/viewer", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var herr util.HttpError
		if err := json.NewDecoder(resp.Body).Decode(&herr); err != nil {
			return nil, fmt.Errorf("authentication check returned unexpected error, code %d", resp.StatusCode)
		}

		return nil, fmt.Errorf("authentication check failed: %s(%d)", herr.Message, herr.Code)
	}

	var out util.ViewerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	return &User{
		ID:              out.ID,
		Username:        out.Username,
		Perms:           out.Perms,
		AuthToken:       token,
		StorageDisabled: out.Settings.ContentAddingDisabled,
	}, nil
}

func (d *Shuttle) AuthRequired(level int) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth, err := util.ExtractAuth(c)
			if err != nil {
				return err
			}

			u, err := d.checkTokenAuth(auth)
			if err != nil {
				return err
			}

			if u.Perms >= level {
				c.Set("user", u)
				return next(c)
			}

			log.Warnw("User not authorized", "user", u.ID, "perms", u.Perms, "required", level)

			return &util.HttpError{
				Code:    401,
				Message: util.ERR_NOT_AUTHORIZED,
			}
		}
	}
}

func withUser(f func(echo.Context, *User) error) func(echo.Context) error {
	return func(c echo.Context) error {
		u, ok := c.Get("user").(*User)
		if !ok {
			return fmt.Errorf("endpoint not called with proper authentication")
		}

		return f(c, u)
	}
}

func (s *Shuttle) ServeAPI(listen string, logging bool) error {
	e := echo.New()

	if logging {
		e.Use(middleware.Logger())
	}

	e.Use(middleware.CORS())

	e.GET("/health", s.handleHealth)

	content := e.Group("/content")
	content.Use(s.AuthRequired(util.PermLevelUser))
	content.POST("/add", withUser(s.handleAdd))
	//content.POST("/add-ipfs", withUser(d.handleAddIpfs))
	//content.POST("/add-car", withUser(d.handleAddCar))

	return e.Start(listen)
}

func (s *Shuttle) handleAdd(c echo.Context, u *User) error {
	ctx := c.Request().Context()

	if u.StorageDisabled {
		return &util.HttpError{
			Code:    400,
			Message: util.ERR_CONTENT_ADDING_DISABLED,
		}
	}

	form, err := c.MultipartForm()
	if err != nil {
		return err
	}
	defer form.RemoveAll()

	mpf, err := c.FormFile("data")
	if err != nil {
		return err
	}

	fname := mpf.Filename
	fi, err := mpf.Open()
	if err != nil {
		return err
	}

	defer fi.Close()

	collection := c.FormValue("collection")

	bsid, bs, err := s.StagingMgr.AllocNew()
	if err != nil {
		return err
	}

	defer func() {
		go func() {
			if err := s.StagingMgr.CleanUp(bsid); err != nil {
				log.Errorf("failed to clean up staging blockstore: %s", err)
			}
		}()
	}()

	bserv := blockservice.New(bs, nil)
	dserv := merkledag.NewDAGService(bserv)

	nd, err := s.importFile(ctx, dserv, fi)
	if err != nil {
		return err
	}

	contid, err := s.createContent(ctx, u, nd.Cid(), fname, collection)
	if err != nil {
		return err
	}

	pin := &Pin{
		Content: contid,
		Cid:     util.DbCID{nd.Cid()},
		UserID:  u.ID,

		Active:  false,
		Pinning: true,
	}

	if err := s.DB.Create(pin).Error; err != nil {
		return err
	}

	if err := s.addDatabaseTrackingToContent(ctx, contid, dserv, bs, nd.Cid()); err != nil {
		return xerrors.Errorf("encountered problem computing object references: %w", err)
	}

	if err := s.dumpBlockstoreTo(ctx, bs, s.Node.Blockstore); err != nil {
		return xerrors.Errorf("failed to move data from staging to main blockstore: %w", err)
	}

	go func() {
		if err := s.Node.Provider.Provide(nd.Cid()); err != nil {
			fmt.Println("providing failed: ", err)
		}
		fmt.Println("providing complete")
	}()
	return c.JSON(200, map[string]string{"cid": nd.Cid().String()})
}

type createContentBody struct {
	Root        cid.Cid  `json:"root"`
	Name        string   `json:"name"`
	Collections []string `json:"collections"`
	Location    string   `json:"location"`
}

type createContentResponse struct {
	ID uint `json:"id"`
}

func (s *Shuttle) createContent(ctx context.Context, u *User, root cid.Cid, fname, collection string) (uint, error) {
	var cols []string
	if collection != "" {
		cols = []string{collection}
	}

	data, err := json.Marshal(createContentBody{
		Root:        root,
		Name:        fname,
		Collections: cols,
		Location:    s.shuttleHandle,
	})
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequest("POST", "https://"+s.estuaryHost+"/content/create", bytes.NewReader(data))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+u.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	var rbody createContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&rbody); err != nil {
		return 0, err
	}

	if rbody.ID == 0 {
		return 0, fmt.Errorf("create content request failed, got back content ID zero")
	}

	return rbody.ID, nil
}

// TODO: mostly copy paste from estuary, dedup code
func (d *Shuttle) doPinning(ctx context.Context, op *pinner.PinningOperation) error {
	ctx, span := Tracer.Start(ctx, "doPinning")
	defer span.End()

	for _, pi := range op.Peers {
		if err := d.Node.Host.Connect(ctx, pi); err != nil {
			log.Warnf("failed to connect to origin node for pinning operation: %s", err)
		}
	}

	bserv := blockservice.New(d.Node.Blockstore, d.Node.Bitswap)
	dserv := merkledag.NewDAGService(bserv)
	dsess := merkledag.NewSession(ctx, dserv)

	if err := d.addDatabaseTrackingToContent(ctx, op.ContId, dsess, d.Node.Blockstore, op.Obj); err != nil {
		// pinning failed, we wont try again. mark pin as dead
		/* maybe its fine if we retry later?
		if err := d.DB.Model(Pin{}).Where("content = ?", op.ContId).UpdateColumns(map[string]interface{}{
			"pinning": false,
		}).Error; err != nil {
			log.Errorf("failed to update failed pin status: %s", err)
		}
		*/

		return err
	}

	/*
		if op.Replace > 0 {
			if err := s.CM.RemoveContent(ctx, op.Replace, true); err != nil {
				log.Infof("failed to remove content in replacement: %d", op.Replace)
			}
		}
	*/

	// this provide call goes out immediately
	if err := d.Node.FullRT.Provide(ctx, op.Obj, true); err != nil {
		log.Infof("provider broadcast failed: %s", err)
	}

	// this one adds to a queue
	if err := d.Node.Provider.Provide(op.Obj); err != nil {
		log.Infof("providing failed: %s", err)
	}

	return nil
}

// TODO: mostly copy paste from estuary, dedup code
func (d *Shuttle) addDatabaseTrackingToContent(ctx context.Context, contid uint, dserv ipld.NodeGetter, bs blockstore.Blockstore, root cid.Cid) error {
	ctx, span := Tracer.Start(ctx, "computeObjRefsUpdate")
	defer span.End()

	var dbpin Pin
	if err := d.DB.First(&dbpin, "content = ?", contid).Error; err != nil {
		return err
	}

	var objects []*Object
	var totalSize int64
	cset := cid.NewSet()

	err := merkledag.Walk(ctx, func(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
		node, err := dserv.Get(ctx, c)
		if err != nil {
			return nil, err
		}

		objects = append(objects, &Object{
			Cid:  util.DbCID{c},
			Size: len(node.RawData()),
		})

		totalSize += int64(len(node.RawData()))

		if c.Type() == cid.Raw {
			return nil, nil
		}

		return node.Links(), nil
	}, root, cset.Visit, merkledag.Concurrent())
	if err != nil {
		return err
	}

	span.SetAttributes(
		attribute.Int64("totalSize", totalSize),
		attribute.Int("numObjects", len(objects)),
	)

	if err := d.DB.CreateInBatches(objects, 300).Error; err != nil {
		return xerrors.Errorf("failed to create objects in db: %w", err)
	}

	if err := d.DB.Model(Pin{}).Where("content = ?", contid).UpdateColumns(map[string]interface{}{
		"active":  true,
		"size":    totalSize,
		"pinning": false,
	}).Error; err != nil {
		return xerrors.Errorf("failed to update content in database: %w", err)
	}

	refs := make([]ObjRef, len(objects))
	for i := range refs {
		refs[i].Pin = dbpin.ID
		refs[i].Object = objects[i].ID
	}

	if err := d.DB.CreateInBatches(refs, 500).Error; err != nil {
		return xerrors.Errorf("failed to create refs: %w", err)
	}

	d.sendPinCompleteMessage(ctx, dbpin.Content, totalSize, objects)

	return nil
}

func (d *Shuttle) onPinStatusUpdate(cont uint, status string) {
	log.Infof("updating pin status: %d %s", cont, status)
	if status == "failed" {
		if err := d.DB.Model(Pin{}).Where("content = ?", cont).UpdateColumns(map[string]interface{}{
			"pinning": false,
			"active":  false,
			"failed":  true,
		}).Error; err != nil {
			log.Errorf("failed to mark pin as failed in database: %s", err)
		}
	}

	go func() {
		if err := d.sendRpcMessage(context.TODO(), &drpc.Message{
			Op: "UpdatePinStatus",
			Params: drpc.MsgParams{
				UpdatePinStatus: &drpc.UpdatePinStatus{
					DBID:   cont,
					Status: status,
				},
			},
		}); err != nil {
			log.Errorf("failed to send pin status update: %s", err)
		}
	}()
}

func (s *Shuttle) refreshPinQueue() error {
	var toPin []Pin
	if err := s.DB.Find(&toPin, "active = false and pinning = true").Error; err != nil {
		return err
	}

	// TODO: this doesnt persist the replacement directives, so a queued
	// replacement, if ongoing during a restart of the node, will still
	// complete the pin when the process comes back online, but it wont delete
	// the old pin.
	// Need to fix this, probably best option is just to add a 'replace' field
	// to content, could be interesting to see the graph of replacements
	// anyways
	log.Infof("refreshing %d pins", len(toPin))
	for _, c := range toPin {
		s.addPinToQueue(c, nil, 0)
	}

	return nil
}

func (s *Shuttle) addPinToQueue(p Pin, peers []peer.AddrInfo, replace uint) {
	op := &pinner.PinningOperation{
		ContId:  p.Content,
		UserId:  p.UserID,
		Obj:     p.Cid.CID,
		Peers:   peers,
		Started: p.CreatedAt,
		Status:  "queued",
		Replace: replace,
	}

	/*

		s.pinLk.Lock()
		// TODO: check if we are overwriting anything here
		s.pinJobs[cont.ID] = op
		s.pinLk.Unlock()
	*/

	s.PinMgr.Add(op)
}

func (s *Shuttle) importFile(ctx context.Context, dserv ipld.DAGService, fi io.Reader) (ipld.Node, error) {
	_, span := Tracer.Start(ctx, "importFile")
	defer span.End()

	spl := chunker.DefaultSplitter(fi)
	return importer.BuildDagFromReader(dserv, spl)
}

func (s *Shuttle) dumpBlockstoreTo(ctx context.Context, from, to blockstore.Blockstore) error {
	ctx, span := Tracer.Start(ctx, "blockstoreCopy")
	defer span.End()

	// TODO: smarter batching... im sure ive written this logic before, just gotta go find it
	keys, err := from.AllKeysChan(ctx)
	if err != nil {
		return err
	}

	var batch []blocks.Block

	for k := range keys {
		blk, err := from.Get(k)
		if err != nil {
			return err
		}

		batch = append(batch, blk)

		if len(batch) > 500 {
			if err := to.PutMany(batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}

	if len(batch) > 0 {
		if err := to.PutMany(batch); err != nil {
			return err
		}
	}

	return nil
}

func (s *Shuttle) getUpdatePacket() (*drpc.ShuttleUpdate, error) {
	var upd drpc.ShuttleUpdate

	upd.PinQueueSize = s.PinMgr.PinQueueSize()

	var st unix.Statfs_t
	if err := unix.Statfs(s.Node.Config.Blockstore, &st); err != nil {
		return nil, err
	}

	upd.BlockstoreSize = st.Blocks * uint64(st.Bsize)
	upd.BlockstoreFree = st.Bavail * uint64(st.Bsize)

	if err := s.DB.Model(Pin{}).Where("active").Count(&upd.NumPins).Error; err != nil {
		return nil, err
	}

	return &upd, nil
}

func (s *Shuttle) handleHealth(c echo.Context) error {
	return c.JSON(200, map[string]string{
		"status": "ok",
	})
}

func (s *Shuttle) Unpin(contid uint) error {
	var pin Pin
	if err := s.DB.First(&pin, "id = ?", contid).Error; err != nil {
		return err
	}

	if err := s.DB.Delete(Pin{}, pin.ID).Error; err != nil {
		return err
	}

	if err := s.DB.Where("pin = ?", pin.ID).Delete(ObjRef{}).Error; err != nil {
		return err
	}

	return s.clearUnreferencedObjects(context.TODO(), pin.ID)
}

func (s *Shuttle) clearUnreferencedObjects(ctx context.Context, pin uint) error {
	s.addPinLk.Lock()
	defer s.addPinLk.Unlock()

	panic("nyi")

}