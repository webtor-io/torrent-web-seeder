package services

import (
	"fmt"
	"net"
	"net/http"
	"runtime/debug"

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
			Name:   WebHostFlag,
			Usage:  "listening host",
			Value:  "",
			EnvVar: "WEB_HOST",
		},
		cli.IntFlag{
			Name:   WebPortFlag,
			Usage:  "http listening port",
			Value:  8080,
			EnvVar: "WEB_PORT",
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

// RecoverMiddleware is a middleware that recovers from panics and logs the error.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// Log the error and stack trace
				log.WithFields(log.Fields{
					"error": fmt.Sprintf("%v", err),
					"stack": string(debug.Stack()),
				}).Error("Recovered from panic")

				// Return 500 Internal Server Error
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
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
	mux.Handle("/", l.Handler(RecoverMiddleware(s.ws), ""))
	log.Infof("serving Web at %v", fmt.Sprintf("%s:%d", s.host, s.port))
	return http.Serve(s.ln, mux)

}

func (s *Web) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
