package services

import (
	"fmt"
	"net"
	"net/http"

	logrusmiddleware "github.com/bakins/logrus-middleware"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	WEB_HOST_FLAG = "host"
	WEB_PORT_FLAG = "port"
)

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:  WEB_HOST_FLAG,
			Usage: "listening host",
			Value: "",
		},
		cli.IntFlag{
			Name:  WEB_PORT_FLAG,
			Usage: "http listening port",
			Value: 8080,
		},
	)
}

type Web struct {
	ws   *WebSeeder
	host string
	port int
	ln   net.Listener
}

func NewWeb(c *cli.Context, ws *WebSeeder) *Web {
	return &Web{
		host: c.String(WEB_HOST_FLAG),
		port: c.Int(WEB_PORT_FLAG),
		ws:   ws,
	}
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil
	}
	s.ln = NewBlockListener(ln, []net.IP{net.ParseIP("127.0.0.1")})

	mux := http.NewServeMux()
	logger := log.New()
	l := logrusmiddleware.Middleware{
		Logger: logger,
	}
	mux.Handle("/", l.Handler(s.ws, ""))
	log.Infof("serving Web at %v", fmt.Sprintf("%s:%d", s.host, s.port))
	return http.Serve(s.ln, mux)

}

func (s *Web) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
