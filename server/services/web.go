package services

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"net"
	"net/http"
	"runtime/debug"

	logrusmiddleware "github.com/bakins/logrus-middleware"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

const (
	WebHostFlag = "host"
	WebPortFlag = "port"
)

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	f = cs.RegisterShutdownFlags(f)
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
	gs   *cs.GracefulServer
}

func NewWeb(c *cli.Context, ws *WebSeeder) *Web {
	return &Web{
		host: c.String(WebHostFlag),
		port: c.Int(WebPortFlag),
		ws:   ws,
		gs:   cs.NewGracefulServer(cs.ShutdownTimeout(c)),
	}
}

var promRecoveredPanics = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "torrent_web_seeder_recovered_panics_total",
	Help: "HTTP handlers that panicked and were answered with a 500 by RecoverMiddleware",
})

func init() {
	prometheus.MustRegister(promRecoveredPanics)
}

// RecoverMiddleware is a middleware that recovers from panics and logs the error.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				promRecoveredPanics.Inc()
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
		return err
	}

	mux := http.NewServeMux()
	logger := log.New()
	l := logrusmiddleware.Middleware{
		Logger: logger,
	}
	mux.Handle("/", l.Handler(RecoverMiddleware(s.ws), ""))
	log.Infof("serving Web at %v", fmt.Sprintf("%s:%d", s.host, s.port))
	return s.gs.Serve(&http.Server{Handler: mux}, ln)
}

// Close drains in-flight requests (WEB_SHUTDOWN_TIMEOUT) before returning.
// Closing only the listener let a terminating pod exit mid-response. It must
// run before the torrent client and stores are closed: run() defers it last.
func (s *Web) Close() {
	s.gs.Close()
}
