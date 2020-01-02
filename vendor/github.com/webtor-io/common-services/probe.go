package services

import (
	"fmt"
	"net"
	"net/http"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

type Probe struct {
	host string
	port int
	ln   net.Listener
}

const (
	PROBE_HOST_FLAG = "probe-host"
	PROBE_PORT_FLAG = "probe-port"
)

func RegisterProbeFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  PROBE_HOST_FLAG,
		Usage: "probe listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  PROBE_PORT_FLAG,
		Usage: "probe listening port",
		Value: 8081,
	})
}

func NewProbe(c *cli.Context) *Probe {
	return &Probe{host: c.String(PROBE_HOST_FLAG), port: c.Int(PROBE_PORT_FLAG)}
}

func (s *Probe) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to probe listen to tcp connection")
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/liveness", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/readiness", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	log.Infof("Serving Probe at %v", addr)
	return http.Serve(ln, mux)
}

func (s *Probe) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
