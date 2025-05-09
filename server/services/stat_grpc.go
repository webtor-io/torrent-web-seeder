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
	pb "github.com/webtor-io/torrent-web-seeder/proto"
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
	StatHostFlag = "stat-host"
	StatPortFlag = "stat-port"
	StatUseFlag  = "use-stat"
)

func RegisterStatFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   StatHostFlag,
			Usage:  "stat listening host",
			Value:  "",
			EnvVar: "STAT_HOST",
		},
		cli.IntFlag{
			Name:   StatPortFlag,
			Usage:  "stat listening port",
			Value:  50051,
			EnvVar: "STAT_PORT",
		},
		cli.BoolTFlag{
			Name:   StatUseFlag,
			Usage:  "enable stat service",
			EnvVar: "USE_STAT",
		},
	)
}

func NewStatGRPC(c *cli.Context, st *Stat) *StatGRPC {
	if !c.BoolT(StatHostFlag) {
		return nil
	}
	return &StatGRPC{
		st:   st,
		host: c.String(StatHostFlag),
		port: c.Int(StatPortFlag),
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
		_ = ss.l.Close()
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
