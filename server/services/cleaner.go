package services

import (
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"time"

	"code.cloudfoundry.org/bytefmt"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sys/unix"
)

const (
	CLEANER_KEEP_FREE_FLAG = "keep-free"
)

func RegisterCleanerFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:  CLEANER_KEEP_FREE_FLAG,
			Usage: "keep free",
			Value: "50GB",
		},
	)
}

type Cleaner struct {
	p        string
	t        *time.Ticker
	cleaning bool
	tm       *TorrentMap
	keep     string
}

type StoreStat struct {
	touch time.Time
	hash  string
}

func NewCleaner(c *cli.Context, tm *TorrentMap) *Cleaner {
	return &Cleaner{
		p:    c.String(DATA_DIR_FLAG),
		tm:   tm,
		keep: c.String(CLEANER_KEEP_FREE_FLAG),
	}
}

func (s *Cleaner) clean() error {
	keep, err := bytefmt.ToBytes(s.keep)
	if err != nil {
		return err
	}
	free := s.getFreeSpace()

	log.Infof("start cleaning free=%.2fG keep=%.2fG", float64(free)/1024/1024/1024, float64(keep)/1024/1024/1024)

	if free > keep {
		log.Info("no need to clean")
		return nil
	}
	stats, err := s.getStats()
	if err != nil {
		return err
	}
	for _, v := range stats {
		log.Infof("drop hash=%v touch=%v", v.hash, v.touch.String())
		err := s.drop(v.hash)
		if err != nil {
			return err
		}
		free := s.getFreeSpace()
		if free > keep {
			return nil
		}
	}
	log.Info("finish cleaning")
	return nil
}

func (s *Cleaner) getFreeSpace() uint64 {
	var stat unix.Statfs_t
	unix.Statfs(s.p, &stat)
	return stat.Bavail * uint64(stat.Bsize)
}

func (s *Cleaner) drop(h string) error {
	os.RemoveAll(s.p + "/" + h)
	os.RemoveAll(s.p + "/" + h + ".touch")
	t, err := s.tm.Get(h)
	if err != nil {
		return err
	}
	t.Drop()
	return nil
}

func (s *Cleaner) getStats() ([]StoreStat, error) {
	res := []StoreStat{}
	ss := map[string]StoreStat{}
	fs, err := ioutil.ReadDir(s.p)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".touch") {
			h := strings.TrimSuffix(f.Name(), ".touch")
			ss[h] = StoreStat{
				hash:  h,
				touch: f.ModTime(),
			}
		} else if f.IsDir() {
			h := f.Name()
			if _, ok := ss[h]; !ok {
				ss[h] = StoreStat{
					hash:  h,
					touch: time.Time{},
				}
			}
		}
	}
	for _, v := range ss {
		res = append(res, v)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].touch.Before(res[j].touch)
	})
	return res, nil
}

func (s *Cleaner) Serve() error {
	log.Info("serving Cleaner")
	s.t = time.NewTicker(30 * time.Second)
	for ; true; <-s.t.C {
		if !s.cleaning {
			s.cleaning = true
			err := s.clean()
			if err != nil {
				log.WithError(err).Errorf("got cleaner error")
			}
			s.cleaning = false
		}
	}
	return nil
}

func (s *Cleaner) Close() {
	if s.t != nil {
		s.t.Stop()
	}
}
