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
	WebHostFlag = "host"
	WebPortFlag = "port"
)

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:  WebHostFlag,
			Usage: "listening host",
			Value: "",
		},
		cli.IntFlag{
			Name:  WebPortFlag,
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
		host: c.String(WebHostFlag),
		port: c.Int(WebPortFlag),
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
		_ = s.ln.Close()
	}
}
