package services

import (
	"os"
	"time"

	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
)

type TouchMap struct {
	lazymap.LazyMap
	p string
}

func NewTouchMap(c *cli.Context) *TouchMap {
	return &TouchMap{
		p: c.String(DataDirFlag),
		LazyMap: lazymap.New(&lazymap.Config{
			Expire: 30 * time.Second,
		}),
	}
}

func (s *TouchMap) touch(h string) error {
	dir, err := GetDir(s.p, h)
	if err != nil {
		return err
	}
	f := dir + ".touch"
	_, err = os.Stat(f)
	if os.IsNotExist(err) {
		file, err := os.Create(f)
		if err != nil {
			return err
		}
		defer func(file *os.File) {
			_ = file.Close()
		}(file)
	} else {
		currentTime := time.Now().Local()
		err = os.Chtimes(f, currentTime, currentTime)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *TouchMap) Touch(h string) error {
	_, err := s.LazyMap.Get(h, func() (interface{}, error) {
		err := s.touch(h)
		return nil, err
	})
	return err
}
