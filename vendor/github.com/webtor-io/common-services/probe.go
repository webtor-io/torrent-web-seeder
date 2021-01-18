package services

import (
	"fmt"
	"net"
	"net/http"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// Probe provides simple HTTP-service for Kubernetes liveness and readiness checking
type Probe struct {
	host string
	port int
	ln   net.Listener
}

const (
	probeHostFlag = "probe-host"
	probePortFlag = "probe-port"
)

// RegisterProbeFlags registers cli flags for Probe
func RegisterProbeFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  probeHostFlag,
		Usage: "probe listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  probePortFlag,
		Usage: "probe listening port",
		Value: 8081,
	})
}

// NewProbe initializes new Probe instance
func NewProbe(c *cli.Context) *Probe {
	return &Probe{host: c.String(probeHostFlag), port: c.Int(probePortFlag)}
}

// Serve serves Probe web service
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

// Close closes Probe web service
func (s *Probe) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
