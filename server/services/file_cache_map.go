package services

import (
	"crypto/sha1"
	"fmt"
	sqlite "github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		fullPath := cachedFilePath(dir, path)
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

// cachedFilePath is where the storage keeps the torrent's file path.
func cachedFilePath(dir, path string) string {
	hexHash := fmt.Sprintf("%x", sha1.Sum([]byte(path)))
	return filepath.Join(dir, "content", hexHash[:2], hexHash)
}

// Get reports where h's file path is cached, for answers that serve no bytes
// (stats, warm-up); serving goes through Open. The answer is kept for 60 s.
func (s *FileCacheMap) Get(h string, path string) (string, error) {
	key := h + path
	return s.LazyMap.Get(key, func() (string, error) {
		return s.get(h, path)
	})
}

// Open opens h's file path for "serve file from cache", or returns a nil file
// when the request has to go to the torrent. The file comes with the
// torrent's directory held shared (see dir_lock.go): no pod on the node
// punches a hole in it until release, which closes the file and lets go of
// the directory and may be called more than once.
//
// Whether the file is served is decided here, under the lock, from
// piece_completion: eviction marks a piece incomplete there before it punches
// it. file_completion only says which pieces to look at. Its row used to be
// the whole check, through Get's 60 s cache, and rows outlive evictions: the
// completion loop inserts from a list taken before the eviction, and another
// pod's loop inserted from memory that never heard of it. In the day to
// 2026-10-04 13:00, 28 of the 31 all-zero pieces Vault rejected came from
// this path.
func (s *FileCacheMap) Open(h string, path string) (f *os.File, release func(), err error) {
	dir, err := GetDir(s.p, h)
	if err != nil {
		return nil, nil, err
	}
	// OpenConn would create a missing db.
	if _, err := os.Stat(filepath.Join(dir, ".torrent.db")); err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, nil, err
	}
	if f, err = openComplete(dir, path); f == nil {
		_ = lock.Close()
		return nil, nil, err
	}
	var once sync.Once
	return f, func() {
		once.Do(func() {
			_ = f.Close()
			_ = lock.Close()
		})
	}, nil
}

// openComplete opens the cached file path if every piece it lies in is
// complete. Called with the directory held.
func openComplete(dir, path string) (*os.File, error) {
	// Read-only: no journal-mode pragma on open, no checkpoint on close.
	db, err := sqlite.OpenConn(filepath.Join(dir, ".torrent.db"), sqlite.OpenReadOnly|sqlite.OpenNoMutex)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	first, last, found := 0, 0, false
	err = sqlitex.Exec(db,
		`select first_piece, last_piece from file_completion where "path"=? and first_piece is not null and last_piece is not null`,
		func(stmt *sqlite.Stmt) error {
			first, last, found = stmt.ColumnInt(0), stmt.ColumnInt(1), true
			return nil
		}, path)
	if err == nil && found {
		complete := 0
		err = sqlitex.Exec(db,
			`select count(*) from piece_completion where "index" between ? and ? and complete`,
			func(stmt *sqlite.Stmt) error {
				complete = stmt.ColumnInt(0)
				return nil
			}, first, last)
		found = complete == last-first+1
	}
	if err != nil {
		// A db no storage has opened since the range columns, or none at all.
		if strings.Contains(err.Error(), "no such") {
			return nil, nil
		}
		return nil, err
	}
	if !found {
		return nil, nil
	}
	f, err := os.Open(cachedFilePath(dir, path))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return f, err
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
