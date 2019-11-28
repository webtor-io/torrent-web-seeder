package services

import (
	"fmt"
	"sync"

	"github.com/urfave/cli"

	ts "bitbucket.org/vintikzzzz/torrent-store/torrent-store"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

type TorrentStore struct {
	cl     ts.TorrentStoreClient
	host   string
	port   int
	conn   *grpc.ClientConn
	mux    sync.Mutex
	err    error
	inited bool
}

const (
	TORRENT_STORE_HOST_FLAG = "torrent-store-host"
	TORRENT_STORE_PORT_FLAG = "torrent-store-port"
)

func RegisterTorrentStoreFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TORRENT_STORE_HOST_FLAG,
		Usage:  "torrent store host",
		Value:  "",
		EnvVar: "TORRENT_STORE_SERVICE_HOST, TORRENT_STORE_HOST",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   TORRENT_STORE_PORT_FLAG,
		Usage:  "torrent store port",
		Value:  50051,
		EnvVar: "TORRENT_STORE_SERVICE_PORT, TORRENT_STORE_PORT",
	})
}

func NewTorrentStore(c *cli.Context) *TorrentStore {
	return &TorrentStore{host: c.String(TORRENT_STORE_HOST_FLAG), port: c.Int(TORRENT_STORE_PORT_FLAG), inited: false}
}

func (s *TorrentStore) get() (ts.TorrentStoreClient, error) {
	log.Info("Initializing TorrentStoreClient")
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	s.conn = conn
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to dial torrent store addr=%v", addr)
	}
	return ts.NewTorrentStoreClient(s.conn), nil
}

func (s *TorrentStore) Get() (ts.TorrentStoreClient, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.cl, s.err
	}
	s.cl, s.err = s.get()
	s.inited = true
	return s.cl, s.err
}

func (s *TorrentStore) Close() {
	if s.conn != nil {
		s.conn.Close()
	}
}
