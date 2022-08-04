package services

import (
	"strconv"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

const (
	TORRENT_CLIENT_POOL_SIZE_FLAG  = "torrent-client-pool-size"
	TORRENT_CLIENT_START_PORT_FLAG = "torrent-client-start-port"
)

func RegisterTorrentClientPoolFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   TORRENT_CLIENT_POOL_SIZE_FLAG,
			Usage:  "torrent client pool size",
			Value:  100,
			EnvVar: "TORRENT_CLIENT_POOL_SIZE",
		},
		cli.IntFlag{
			Name:   TORRENT_CLIENT_START_PORT_FLAG,
			Usage:  "torrent-client-start-port",
			Value:  42069,
			EnvVar: "TORRENT_CLIENT_START_PORT",
		},
	)
}

type TorrentClientPool struct {
	clients map[int]*TorrentClient
}

func NewTorrentClientPool(c *cli.Context) (*TorrentClientPool, error) {
	clients := map[int]*TorrentClient{}
	size := c.Int(TORRENT_CLIENT_POOL_SIZE_FLAG)
	startPort := c.Int(TORRENT_CLIENT_START_PORT_FLAG)
	for i := 0; i < size; i++ {
		tc, err := NewTorrentClient(c, startPort+i)
		if err != nil {
			return nil, err
		}
		clients[i] = tc
	}
	return &TorrentClientPool{
		clients: clients,
	}, nil
}

func (s *TorrentClientPool) Get(h string) (*TorrentClient, error) {
	hex := h[0:5]
	num, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse hex from infohash=%v", h)
	}
	total := int64(1048575)
	k := int(float64(num) / float64(total) * float64(len(s.clients)))
	return s.clients[int(k)], nil
}
