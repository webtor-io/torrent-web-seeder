package services

import (
	"os"
	"time"

	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
)

type TouchMap struct {
	lazymap.LazyMap[bool]
	p string
}

func NewTouchMap(c *cli.Context) *TouchMap {
	return &TouchMap{
		p: c.String(DataDirFlag),
		LazyMap: lazymap.New[bool](&lazymap.Config{
			Expire: 30 * time.Second,
		}),
	}
}

func (s *TouchMap) touch(h string) (bool, error) {
	dir, err := GetDir(s.p, h)
	if err != nil {
		return false, err
	}
	f := dir + ".touch"
	_, err = os.Stat(f)
	if os.IsNotExist(err) {
		file, err := os.Create(f)
		if err != nil {
			return false, err
		}
		defer func(file *os.File) {
			_ = file.Close()
		}(file)
	} else {
		currentTime := time.Now().Local()
		err = os.Chtimes(f, currentTime, currentTime)
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *TouchMap) Touch(h string) (bool, error) {
	return s.LazyMap.Get(h, func() (bool, error) {
		return s.touch(h)
	})
}
