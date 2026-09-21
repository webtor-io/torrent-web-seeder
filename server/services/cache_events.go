package services

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// Cache events: the seeder says when a whole file becomes complete on its disk
// and when it stops being so. It is the only party that knows the moment. A
// consumer that asks at the START of a transfer ("is it cached?") hears "no",
// the transfer then completes the file, and nobody asks again -- so an index of
// what starts instantly stayed blind to exactly the content just fetched.
//
//	resource.cached    {"resource_id": "<infohash>", "file_idx": 3}
//	resource.uncached  {"resource_id": "<infohash>", "file_idx": 3}
//
// file_idx is the file's position in the torrent's file order (0 for a
// single-file torrent) -- the number the rest of the platform calls the file
// index.
//
// Best-effort by design. An event lost to a dead NATS or a killed pod is not
// repaired here: consumers keep an age limit on what they were told.
const (
	natsServiceHostFlag = "nats-service-host"
	natsServicePortFlag = "nats-service-port"

	cachedSubject   = "resource.cached"
	uncachedSubject = "resource.uncached"
)

func RegisterCacheEventsFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   natsServiceHostFlag,
			Usage:  "nats service host; cache events are published when set",
			Value:  "",
			EnvVar: "NATS_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   natsServicePortFlag,
			Usage:  "nats service port",
			Value:  4222,
			EnvVar: "NATS_SERVICE_PORT",
		},
	)
}

type cachePublisher interface {
	Publish(subject string, data []byte) error
}

// CacheEvents publishes; a nil *CacheEvents is valid and publishes nothing.
type CacheEvents struct {
	p  cachePublisher
	nc *nats.Conn
}

type cacheEventMsg struct {
	ResourceID string `json:"resource_id"`
	FileIdx    int    `json:"file_idx"`
}

// NewCacheEvents returns nil when NATS is not configured -- and says so, since
// the absence is otherwise invisible: everything works, the index downstream
// just never hears from this seeder.
func NewCacheEvents(c *cli.Context) *CacheEvents {
	host := c.String(natsServiceHostFlag)
	if host == "" {
		log.Info("cache events are off: nats service host is not set")
		return nil
	}
	url := fmt.Sprintf("nats://%s:%d", host, c.Int(natsServicePortFlag))
	// The seeder must start and serve whether or not NATS is there: connect
	// in the background and keep trying. While disconnected, publishes go to
	// the client's reconnect buffer and past it fail -- logged, never fatal.
	nc, err := nats.Connect(url,
		nats.Name("torrent-web-seeder"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(5*time.Second),
	)
	if err != nil {
		log.WithError(err).Warn("cache events are off: failed to set up nats connection")
		return nil
	}
	log.WithField("url", url).Info("cache events are published")
	return &CacheEvents{p: nc, nc: nc}
}

func (s *CacheEvents) Cached(hash string, fileIdx int) { s.publish(cachedSubject, hash, fileIdx) }

func (s *CacheEvents) Uncached(hash string, fileIdx int) { s.publish(uncachedSubject, hash, fileIdx) }

func (s *CacheEvents) publish(subject string, hash string, fileIdx int) {
	if s == nil || s.p == nil || hash == "" || fileIdx < 0 {
		return
	}
	b, err := json.Marshal(cacheEventMsg{ResourceID: hash, FileIdx: fileIdx})
	if err != nil {
		return
	}
	if err := s.p.Publish(subject, b); err != nil {
		log.WithError(err).WithField("subject", subject).WithField("hash", hash).Warn("failed to publish cache event")
	}
}

func (s *CacheEvents) Close() {
	if s != nil && s.nc != nil {
		s.nc.Close()
	}
}
