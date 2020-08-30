package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"goatac/cot"
	"goatac/model"
	"goatac/xml"
)

var (
	gitRevision = "unknown"
	gitBranch   = "unknown"
)

type Msg struct {
	event *cot.Event
	dat   []byte
}

type App struct {
	Logger  *zap.SugaredLogger
	tcpport int
	udpport int

	clients map[string]*ClientHandler
	points  map[string]*model.Unit
	ctx     context.Context
	uid     string
	ch      chan *Msg
	logging bool

	clientMx sync.RWMutex
	pointMx  sync.RWMutex
}

func NewApp(tcpport, udpport int, logger *zap.SugaredLogger) *App {
	return &App{
		Logger:   logger,
		tcpport:  tcpport,
		udpport:  udpport,
		ch:       make(chan *Msg, 20),
		clients:  make(map[string]*ClientHandler, 0),
		points:   make(map[string]*model.Unit, 0),
		uid:      uuid.New().String(),
		clientMx: sync.RWMutex{},
		pointMx:  sync.RWMutex{},
	}
}

func (app *App) Run() {
	var cancel context.CancelFunc

	app.ctx, cancel = context.WithCancel(context.Background())

	go func() {
		if err := app.ListenUDP(fmt.Sprintf(":%d", app.udpport)); err != nil {
			panic(err)
		}
	}()

	go func() {
		if err := app.ListenTCP(fmt.Sprintf(":%d", app.tcpport)); err != nil {
			panic(err)
		}
	}()

	go func() {
		if err := NewHttp(app, ":8080").Serve(); err != nil {
			panic(err)
		}
	}()

	go app.EventProcessor()

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	<-c
	app.Logger.Info("exiting...")
	cancel()
}

func (app *App) AddClient(uid string, cl *ClientHandler) {
	app.clientMx.Lock()
	defer app.clientMx.Unlock()

	app.Logger.Infof("new client: %s", uid)
	app.clients[uid] = cl
}

func (app *App) RemoveClient(uid string) {
	app.clientMx.Lock()
	defer app.clientMx.Unlock()

	if _, ok := app.clients[uid]; ok {
		app.Logger.Infof("remove client: %s", uid)
		delete(app.clients, uid)
	}
}

func (app *App) AddUnit(uid string, u *model.Unit) {
	app.pointMx.Lock()
	defer app.pointMx.Unlock()

	app.points[uid] = u
}

func (app *App) RemovePoint(uid string) {
	app.pointMx.Lock()
	defer app.pointMx.Unlock()

	if _, ok := app.points[uid]; ok {
		delete(app.points, uid)
	}
}

func (app *App) EventProcessor() {
	for {
		msg := <-app.ch

		if msg.event == nil {
			continue
		}

		if app.logging {
			if f, err := os.OpenFile(msg.event.Type+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666); err == nil {
				f.Write(msg.dat)
				f.Write([]byte{10})
				f.Close()
			} else {
				fmt.Println(err)
			}
		}

		switch {
		case msg.event.Type == "t-x-c-t":
			// ping
			app.Logger.Debugf("ping from %s", msg.event.Uid)
			app.SendTo(cot.MakePong(), msg.event.Uid)
			continue
		case msg.event.IsChat():
			app.Logger.Infof("chat %s %s", msg.event.Detail.Chat, msg.event.GetText())
		case strings.HasPrefix(msg.event.Type, "a-"):
			app.Logger.Debugf("pos %s (%s)", msg.event.Uid, msg.event.Detail.Contact.Callsign)
			app.AddUnit(msg.event.Uid, model.FromEvent(msg.event))
		case strings.HasPrefix(msg.event.Type, "b-"):
			app.Logger.Debugf("point %s (%s)", msg.event.Uid, msg.event.Detail.Contact.Callsign)
			app.AddUnit(msg.event.Uid, model.FromEvent(msg.event))
		default:
			app.Logger.Debugf("event: %s", msg.event)
		}

		if len(msg.event.GetCallsignTo()) > 0 {
			for _, s := range msg.event.GetCallsignTo() {
				app.SendMsgToCallsign(msg.dat, s)
			}
		} else {
			app.SendMsgToAll(msg.dat, msg.event.Uid)
		}
	}
}

func (app *App) SendToAll(evt *cot.Event, author string) {
	msg, err := xml.Marshal(evt)

	if err != nil {
		app.Logger.Errorf("marshalling error: %v", err)
		return
	}

	app.SendMsgToAll(msg, author)
}

func (app *App) SendMsgToAll(msg []byte, author string) {
	app.clientMx.RLock()
	defer app.clientMx.RUnlock()

	for uid, h := range app.clients {
		if uid != author {
			h.AddMsg(msg)
		}
	}
}

func (app *App) SendTo(evt *cot.Event, uid string) {
	msg, err := xml.Marshal(evt)

	if err != nil {
		app.Logger.Errorf("marshalling error: %v", err)
		return
	}

	app.SendMsgTo(msg, uid)
}

func (app *App) SendToCallsign(evt *cot.Event, callsign string) {
	msg, err := xml.Marshal(evt)

	if err != nil {
		app.Logger.Errorf("marshalling error: %v", err)
		return
	}

	app.SendMsgToCallsign(msg, callsign)
}

func (app *App) SendMsgTo(msg []byte, uid string) {
	app.clientMx.RLock()
	defer app.clientMx.RUnlock()

	if h, ok := app.clients[uid]; ok {
		h.AddMsg(msg)
	}
}

func (app *App) SendMsgToCallsign(msg []byte, callsign string) {
	app.clientMx.RLock()
	defer app.clientMx.RUnlock()

	for _, h := range app.clients {
		if h.Callsign == callsign {
			h.AddMsg(msg)
		}
	}
}

func main() {
	fmt.Printf("version %s:%s\n", gitBranch, gitRevision)

	var tcpPort = flag.Int("tcp_port", 8089, "port for tcp")
	var udpPort = flag.Int("udp_port", 4242, "port for udp")
	var logging = flag.Bool("logging", false, "save all events to files")

	flag.Parse()

	cfg := zap.NewDevelopmentConfig()
	logger, _ := cfg.Build()
	defer logger.Sync()

	app := NewApp(*tcpPort, *udpPort, logger.Sugar())
	app.logging = *logging
	app.Run()
}
