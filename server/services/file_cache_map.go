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
	// OpenConn would create a missing db.
	if _, err := os.Stat(filepath.Join(dir, ".torrent.db")); os.IsNotExist(err) {
		return "", nil
	}
	if ok, err := fileComplete(dir, path, true); !ok {
		return "", err
	}
	fullPath := cachedFilePath(dir, path)
	if _, err := os.Stat(fullPath); err == nil {
		return fullPath, nil
	} else if os.IsNotExist(err) {
		return "", nil
	} else {
		return "", err
	}
}

// cachedFilePath is where the storage keeps the torrent's file path.
func cachedFilePath(dir, path string) string {
	hexHash := fmt.Sprintf("%x", sha1.Sum([]byte(path)))
	return filepath.Join(dir, "content", hexHash[:2], hexHash)
}

// Get reports where h's file path is cached, for answers that serve no bytes
// (stats, warm-up, done): whether the file needs the swarm. It checks the
// file's pieces as Open does, without the lock, as the answer only advises,
// and not the eviction gate: a complete file of an evicting torrent is served
// by the torrent path, but from disk. Unlike Open it takes a row without the
// range at its word (see completeFiles). The answer is kept for 60 s.
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
// A torrent that has been opened with eviction on (its dir has the eviction
// gate) is left to the torrent. A stream holds the directory for its whole
// response, and one that keeps moving is never cut: such streams held
// evicting torrents for up to 11 h, 172 h a day over 79 node+hash pairs
// (2026-10-03..04), with every eviction of the torrent on the node, the
// serving pod's own included, put off behind them. The torrent path reads
// through the evicted-piece guard, and its storage's lock does not stand in
// the way of its own evictions. A torrent nobody evicts has nobody waiting on
// its stream, short of a pod that starts evicting it during the stream.
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
	if _, err := os.Stat(filepath.Join(dir, evictGateName)); !os.IsNotExist(err) {
		return nil, nil, nil
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
	if ok, err := fileComplete(dir, path, false); !ok {
		return nil, err
	}
	f, err := os.Open(cachedFilePath(dir, path))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return f, err
}

// fileComplete reports whether every piece path lies in is complete in dir's
// piece_completion; legacy: see completeFiles.
func fileComplete(dir, path string, legacy bool) (bool, error) {
	n, err := completeFiles(dir, `"path" = ?`, path, legacy)
	return n > 0, err
}

// completeFiles counts dir's file_completion rows matching where (one ?,
// arg) whose pieces, first_piece to last_piece, are all complete in
// piece_completion. One query: IsDirComplete ran two per file, 35 ms for
// 10000 files on every ?stats or ?done of the root.
//
// A row without the range is from a storage older than the range columns,
// and 8e3afa1 writes such rows still: over a row of this one's during a
// rollout, and anew after a rollback. A db no storage has opened since has
// only such rows (and no columns). legacy counts them complete, as the row
// alone counted before; the answers that only advise (Get, IsDirComplete)
// take it, or every file cached before the rollout would lose "cached" until
// a pod of this version loads its torrent. The cache path does not: it serves
// the bytes, and production's zeros sat under such rows.
func completeFiles(dir, where string, arg any, legacy bool) (n int, err error) {
	// Read-only: no journal-mode pragma on open, no checkpoint on close.
	db, err := sqlite.OpenConn(filepath.Join(dir, ".torrent.db"), sqlite.OpenReadOnly|sqlite.OpenNoMutex)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	count := func(stmt *sqlite.Stmt) error {
		n = stmt.ColumnInt(0)
		return nil
	}
	// A NULL range compares as NULL: not complete.
	complete := `(select count(*) from piece_completion p where p."index" between f.first_piece and f.last_piece and p.complete)
		= f.last_piece - f.first_piece + 1`
	if legacy {
		complete = `(f.first_piece is null or ` + complete + `)`
	}
	err = sqlitex.Exec(db, `select count(*) from file_completion f where `+where+` and `+complete, count, arg)
	if err != nil && legacy && strings.Contains(err.Error(), "no such column") {
		err = sqlitex.Exec(db, `select count(*) from file_completion f where `+where, count, arg)
	}
	if err != nil && strings.Contains(err.Error(), "no such") {
		return 0, nil
	}
	return n, err
}

// IsDirComplete checks if all files under a directory (or all torrent files
// for root) are complete, by the same check as Get. expectedFiles is the total
// number of files expected (from torrent metadata).
func (s *FileCacheMap) IsDirComplete(h string, dirPath string, expectedFiles int) (bool, error) {
	if expectedFiles <= 0 {
		return false, nil
	}
	dir, err := GetDir(s.p, h)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir + "/.torrent.db"); os.IsNotExist(err) {
		return false, nil
	}
	pattern := "%" // root: every file
	if dirPath != "" {
		pattern = dirPath + "/%"
	}
	n, err := completeFiles(dir, `"path" like ?`, pattern, true)
	return n >= expectedFiles, err
}
