package services

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	InputFlag string = "input"
)

func RegisterFileStoreFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   InputFlag,
			Usage:  "torrent file path",
			EnvVar: "INPUT",
		},
	)
}

type FileStoreMap struct {
	p     string
	once  sync.Once
	infos map[string]*metainfo.MetaInfo
	err   error
}

func NewFileStoreMap(c *cli.Context) *FileStoreMap {
	return &FileStoreMap{
		p: c.String(InputFlag),
	}
}

func (s *FileStoreMap) loadFiles() (map[string]*metainfo.MetaInfo, error) {
	path := s.p
	usr, _ := user.Current()
	dir := usr.HomeDir
	if path == "~" {
		path = dir
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(dir, path[2:])
	}
	m := map[string]*metainfo.MetaInfo{}
	if path == "" {
		return m, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		fs, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".torrent") {
				mi, err := metainfo.LoadFromFile(path + "/" + f.Name())
				if err != nil {
					log.WithError(err).Error("failed to load torrent")
					continue
				}
				m[mi.HashInfoBytes().HexString()] = mi
			}
		}
	} else {
		mi, err := metainfo.LoadFromFile(path)
		if err != nil {
			return nil, err
		}
		m[mi.HashInfoBytes().HexString()] = mi
	}
	return m, nil
}

func (s *FileStoreMap) Get(h string) (*metainfo.MetaInfo, error) {
	s.once.Do(func() {
		s.infos, s.err = s.loadFiles()
	})
	if s.err != nil {
		return nil, s.err
	}
	i := s.infos[h]
	return i, nil
}

func (s *FileStoreMap) List() ([]string, error) {
	hs := []string{}
	s.once.Do(func() {
		s.infos, s.err = s.loadFiles()
	})
	if s.err != nil {
		return hs, s.err
	}
	for h := range s.infos {
		hs = append(hs, h)
	}
	return hs, nil
}
