package services

import (
	"fmt"
	"sync"

	"github.com/urfave/cli"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	TorrentStoreHostFlag = "torrent-store-host"
	TorrentStorePortFlag = "torrent-store-port"
)

func RegisterTorrentStoreFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   TorrentStoreHostFlag,
			Usage:  "torrent store host",
			Value:  "",
			EnvVar: "TORRENT_STORE_SERVICE_HOST, TORRENT_STORE_HOST",
		},
		cli.IntFlag{
			Name:   TorrentStorePortFlag,
			Usage:  "torrent store port",
			Value:  50051,
			EnvVar: "TORRENT_STORE_SERVICE_PORT, TORRENT_STORE_PORT",
		},
	)
}

func NewTorrentStore(c *cli.Context) *TorrentStore {
	return &TorrentStore{
		host: c.String(TorrentStoreHostFlag),
		port: c.Int(TorrentStorePortFlag),
	}
}

func (s *TorrentStore) get() (ts.TorrentStoreClient, error) {
	log.Info("initializing TorrentStoreClient")
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	s.conn = conn
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial torrent store addr=%v", addr)
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
		_ = s.conn.Close()
	}
}
