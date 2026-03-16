package services

import (
	"crypto/sha1"
	"fmt"
	sqlite "github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
)

type FileCacheMap struct {
	lazymap.LazyMap[string]
	p string
}

func NewFileCacheMap(c *cli.Context) *FileCacheMap {
	return &FileCacheMap{
		p: c.String(DataDirFlag),
		LazyMap: lazymap.New[string](&lazymap.Config{
			Expire: 60 * time.Second,
		}),
	}
}

func (s *FileCacheMap) get(h string, path string) (string, error) {
	dir, err := GetDir(s.p, h)
	if err != nil {
		return "", err
	}
	f := dir + "/.torrent.db"
	_, err = os.Stat(f)
	if os.IsNotExist(err) {
		return "", nil
	}
	db, err := sqlite.OpenConn(f, 0)
	if err != nil {
		return "", err
	}
	defer func(db *sqlite.Conn) {
		_ = db.Close()
	}(db)
	var complete bool
	err = sqlitex.Exec(
		db, `select "path" from file_completion where "path"=?`,
		func(stmt *sqlite.Stmt) error {
			complete = stmt.DataCount() > 0
			return nil
		},
		path)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return "", nil
		}
		return "", err
	}
	if complete {
		hash := sha1.Sum([]byte(path))
		hexHash := fmt.Sprintf("%x", hash)
		subPath := hexHash[:2]
		fullPath := filepath.Join(dir, "content", subPath, hexHash)
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath, nil
		} else if os.IsNotExist(err) {
			return "", nil
		} else {
			return "", err
		}
	}
	return "", nil
}

func (s *FileCacheMap) Get(h string, path string) (string, error) {
	key := h + path
	return s.LazyMap.Get(key, func() (string, error) {
		return s.get(h, path)
	})
}

// IsDirComplete checks if all files under a directory (or all torrent files for root) are in file_completion.
// expectedFiles is the total number of files expected (from torrent metadata).
func (s *FileCacheMap) IsDirComplete(h string, dirPath string, expectedFiles int) (bool, error) {
	if expectedFiles <= 0 {
		return false, nil
	}
	dir, err := GetDir(s.p, h)
	if err != nil {
		return false, err
	}
	f := dir + "/.torrent.db"
	if _, err := os.Stat(f); os.IsNotExist(err) {
		return false, nil
	}
	db, err := sqlite.OpenConn(f, 0)
	if err != nil {
		return false, err
	}
	defer func(db *sqlite.Conn) {
		_ = db.Close()
	}(db)

	var completedCount int
	if dirPath == "" {
		// Root: count all completed files
		err = sqlitex.Exec(
			db, `select count(*) from file_completion`,
			func(stmt *sqlite.Stmt) error {
				completedCount = stmt.ColumnInt(0)
				return nil
			})
	} else {
		// Directory: count completed files with matching prefix
		err = sqlitex.Exec(
			db, `select count(*) from file_completion where "path" like ?`,
			func(stmt *sqlite.Stmt) error {
				completedCount = stmt.ColumnInt(0)
				return nil
			},
			dirPath+"/%")
	}
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return false, nil
		}
		return false, err
	}
	return completedCount >= expectedFiles, nil
}
