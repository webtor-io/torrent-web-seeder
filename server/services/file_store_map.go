package services

import (
	"io/ioutil"
	"os"
	"strings"
	"sync"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/urfave/cli"
)

const (
	INPUT_FLAG string = "input"
)

func RegisterFileStoreFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   INPUT_FLAG,
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
		p: c.String(INPUT_FLAG),
	}
}

func (s *FileStoreMap) loadFiles() (map[string]*metainfo.MetaInfo, error) {
	m := map[string]*metainfo.MetaInfo{}
	if s.p == "" {
		return m, nil
	}
	fi, err := os.Stat(s.p)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		fs, err := ioutil.ReadDir(s.p)
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".torrent") {
				mi, err := metainfo.LoadFromFile(s.p + "/" + f.Name())
				if err != nil {
					return nil, err
				}
				m[mi.HashInfoBytes().HexString()] = mi
			}
		}
	} else {
		mi, err := metainfo.LoadFromFile(s.p)
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
