package services

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const DebugStatusPortFlag = "debug-status-port"

func RegisterDebugStatusFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   DebugStatusPortFlag,
			Usage:  "port of the torrent client status page on 127.0.0.1 (reach it with kubectl port-forward); 0 disables",
			Value:  8084,
			EnvVar: "DEBUG_STATUS_PORT",
		},
	)
}

// DebugStatus serves the torrent library's own status report, with every
// peer connection's flags (who is interested, who is choking whom), which
// nothing else exposes. GET /status?hash=<infohash> returns that torrent's
// block only. It listens on 127.0.0.1: the report names peer addresses and
// must not be reachable through the proxies.
type DebugStatus struct {
	tc   *TorrentClient
	port int
	srv  *http.Server
}

func NewDebugStatus(c *cli.Context, tc *TorrentClient) *DebugStatus {
	port := c.Int(DebugStatusPortFlag)
	if port == 0 {
		return nil
	}
	return &DebugStatus{tc: tc, port: port}
}

func (s *DebugStatus) Serve() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.status)
	s.srv = &http.Server{Handler: mux}
	log.Infof("serving torrent client status at %v", ln.Addr())
	err = s.srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *DebugStatus) status(w http.ResponseWriter, r *http.Request) {
	cl, err := s.tc.Get()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var buf bytes.Buffer
	cl.WriteStatus(&buf)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	h := strings.ToLower(r.URL.Query().Get("hash"))
	if h == "" {
		_, _ = w.Write(buf.Bytes())
		return
	}
	_, _ = w.Write([]byte(torrentStatusBlock(buf.String(), h)))
}

// torrentStatusBlock cuts one torrent's block, peers included, out of
// Client.WriteStatus output: blocks are separated by a blank line and name
// their torrent with an "Infohash: <hex>" line.
func torrentStatusBlock(status, hash string) string {
	for _, b := range strings.Split(status, "\n\n") {
		if strings.Contains(b, "Infohash: "+hash) {
			return b + "\n"
		}
	}
	return ""
}

func (s *DebugStatus) Close() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}
