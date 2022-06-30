package services

import (
	"fmt"
	"net"
	"sync"

	"github.com/pkg/errors"
	"github.com/urfave/cli"

	"google.golang.org/grpc"

	"google.golang.org/grpc/reflection"

	log "github.com/sirupsen/logrus"
	pb "github.com/webtor-io/torrent-web-seeder/torrent-web-seeder"
)

type StatGRPC struct {
	s    *grpc.Server
	host string
	port int
	l    net.Listener
	err  error
	st   *Stat
	once sync.Once
}

const (
	STAT_HOST_FLAG = "stat-host"
	STAT_PORT_FLAG = "stat-port"
)

func RegisterStatFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:  STAT_HOST_FLAG,
			Usage: "stat listening host",
			Value: "",
		},
		cli.IntFlag{
			Name:  STAT_PORT_FLAG,
			Usage: "stat listening port",
			Value: 50051,
		},
	)
}

func NewStatGRPC(c *cli.Context, st *Stat) *StatGRPC {
	return &StatGRPC{
		st:   st,
		host: c.String(STAT_HOST_FLAG),
		port: c.Int(STAT_PORT_FLAG),
	}
}

func (ss *StatGRPC) Serve() error {
	s, err := ss.Get()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", ss.host, ss.port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "failed to listen to tcp connection")
	}
	ss.l = l
	log.Infof("serving Stat at %v", addr)
	return s.Serve(l)
}

func (ss *StatGRPC) Close() {
	if ss.l != nil {
		ss.l.Close()
	}
}

func (ss *StatGRPC) get() (*grpc.Server, error) {
	log.Info("initializing Stat")
	s := grpc.NewServer()
	pb.RegisterTorrentWebSeederServer(s, ss.st)
	reflection.Register(s)
	return s, nil
}

func (s *StatGRPC) Get() (*grpc.Server, error) {
	s.once.Do(func() {
		s.s, s.err = s.get()
	})
	return s.s, s.err
}
