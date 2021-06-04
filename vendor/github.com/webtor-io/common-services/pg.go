package services

import (
	"crypto/tls"
	"fmt"
	"sync"

	"github.com/go-pg/pg/v10"
	"github.com/urfave/cli"
)

const (
	pgHostFlag     = "postgres-host"
	pgPortFlag     = "postgres-port"
	pgUserFlag     = "postgres-user"
	pgPasswordFlag = "postgres-password"
	pgDatabaseFlag = "postgres-database"
	pgSSLFlag      = "postgres-ssl"
)

func RegisterPGFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   pgHostFlag,
			Usage:  "postgres host",
			Value:  "localhost",
			EnvVar: "PG_HOST",
		},
		cli.IntFlag{
			Name:   pgPortFlag,
			Usage:  "postgres port",
			Value:  5432,
			EnvVar: "PG_PORT",
		},
		cli.StringFlag{
			Name:   pgUserFlag,
			Usage:  "postgres user",
			Value:  "webhook",
			EnvVar: "PG_USER",
		},
		cli.StringFlag{
			Name:   pgPasswordFlag,
			Usage:  "postgres password",
			Value:  "",
			EnvVar: "PG_PASSWORD",
		},
		cli.StringFlag{
			Name:   pgDatabaseFlag,
			Usage:  "postgres database",
			Value:  "webhook",
			EnvVar: "PG_DATABASE",
		},
		cli.BoolFlag{
			Name:   pgSSLFlag,
			Usage:  "postgres ssl",
			EnvVar: "PG_SSL",
		},
	)
}

type PG struct {
	host     string
	port     int
	user     string
	password string
	database string
	ssl      bool
	db       *pg.DB
	mux      sync.Mutex
	inited   bool
}

func NewPG(c *cli.Context) *PG {
	return &PG{
		host:     c.String(pgHostFlag),
		port:     c.Int(pgPortFlag),
		user:     c.String(pgUserFlag),
		password: c.String(pgPasswordFlag),
		database: c.String(pgDatabaseFlag),
		ssl:      c.Bool(pgSSLFlag),
	}
}

func (s *PG) get() *pg.DB {
	opts := &pg.Options{}
	opts.Addr = fmt.Sprintf("%v:%v", s.host, s.port)
	opts.User = s.user
	opts.Password = s.password
	opts.Database = s.database
	if s.ssl {
		opts.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	return pg.Connect(opts)
}

func (s *PG) Get() *pg.DB {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.db
	}
	s.db = s.get()
	s.inited = true
	return s.db
}

func (s *PG) Close() {
	if s.db != nil {
		s.db.Close()
	}
}
