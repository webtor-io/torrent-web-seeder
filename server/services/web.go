package services

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"bitbucket.org/vintikzzzz/gracenet"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	WEB_HOST_FLAG  = "host"
	WEB_PORT_FLAG  = "port"
	WEB_GRACE_FLAG = "grace"
)

func RegisterWebFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  WEB_HOST_FLAG,
		Usage: "listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  WEB_PORT_FLAG,
		Usage: "http listening port",
		Value: 8080,
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   WEB_GRACE_FLAG,
		Usage:  "grace in seconds",
		Value:  600,
		EnvVar: "GRACE",
	})
}

type Web struct {
	ws     *WebSeeder
	host   string
	port   int
	grace  int
	gln    *gracenet.GraceListener
	mux    sync.Mutex
	err    error
	inited bool
}

func NewWeb(c *cli.Context, ws *WebSeeder) *Web {
	return &Web{host: c.String(WEB_HOST_FLAG), port: c.Int(WEB_PORT_FLAG), ws: ws, grace: c.Int(WEB_GRACE_FLAG), inited: false}
}

func (s *Web) getListener() (*gracenet.GraceListener, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.gln, s.err
	}
	s.inited = true
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.err = errors.Wrap(err, "Failed to listen to tcp connection")
		s.gln = nil
		return s.gln, s.err
	}
	s.gln = gracenet.NewGraceListener(NewBlockListener(ln, []net.IP{net.ParseIP("127.0.0.1")}), time.Duration(s.grace)*time.Second)
	s.err = nil
	return s.gln, s.err
}

func (s *Web) Serve() error {
	ln, err := s.getListener()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/", s.ws)
	log.Infof("Serving Web at %v", fmt.Sprintf("%s:%d", s.host, s.port))
	return http.Serve(ln, mux)

}

func (s *Web) Close() {
	if s.gln != nil {
		s.gln.Close()
	}
}

func (s *Web) Expire() (<-chan bool, error) {

	ln, err := s.getListener()
	if err != nil {
		return nil, err
	}
	return ln.Expire(), nil
}
