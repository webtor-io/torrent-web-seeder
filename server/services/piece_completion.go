package services

import (
	"errors"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type completions struct {
	pieces         []bool
	completedCount int
	completedFiles map[string]bool
	completed      bool
	mux            sync.Mutex
	info           *metainfo.Info
}

func (s *completions) Complete(index int) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.pieces[index] {
		return
	}
	s.completedCount++
	s.pieces[index] = true
	s.completed = s.completedCount == len(s.pieces)
}

// isCompleted is for the completion loop, which runs beside the hashers that
// call Complete and Uncomplete: the flag is read under the same lock.
func (s *completions) isCompleted() bool {
	s.mux.Lock()
	defer s.mux.Unlock()
	return s.completed
}

// Uncomplete marks a piece as incomplete and invalidates file-level completion
// for any files that include this piece.
func (s *completions) Uncomplete(index int) []string {
	s.mux.Lock()
	defer s.mux.Unlock()
	if !s.pieces[index] {
		return nil
	}
	s.completedCount--
	s.pieces[index] = false
	s.completed = false
	// Find and reset file completions affected by this piece.
	var affectedFiles []string
	if len(s.info.Files) == 0 {
		// Single-file torrent.
		delete(s.completedFiles, s.info.Name)
		affectedFiles = append(affectedFiles, s.info.Name)
	} else {
		offset := 0
		for _, f := range s.info.Files {
			path := s.info.Name + "/" + strings.Join(f.Path, "/")
			startPiece := offset / int(s.info.PieceLength)
			endPiece := (offset + int(f.Length)) / int(s.info.PieceLength)
			offset += int(f.Length)
			if index >= startPiece && index <= endPiece {
				if s.completedFiles[path] {
					delete(s.completedFiles, path)
					affectedFiles = append(affectedFiles, path)
				}
			}
		}
	}
	return affectedFiles
}

// IsPieceInCompletedFile returns true if the piece belongs to a file
// that is fully downloaded (tracked in completedFiles).
func (s *completions) IsPieceInCompletedFile(index int) bool {
	s.mux.Lock()
	defer s.mux.Unlock()
	if len(s.completedFiles) == 0 {
		return false
	}
	if len(s.info.Files) == 0 {
		// Single-file torrent.
		return s.completedFiles[s.info.Name]
	}
	offset := 0
	for _, f := range s.info.Files {
		path := s.info.Name + "/" + strings.Join(f.Path, "/")
		startPiece := offset / int(s.info.PieceLength)
		endPiece := (offset + int(f.Length)) / int(s.info.PieceLength)
		offset += int(f.Length)
		if index >= startPiece && index <= endPiece {
			if s.completedFiles[path] {
				return true
			}
		}
	}
	return false
}

func (s *completions) GetCompletedFiles() []string {
	s.mux.Lock()
	defer s.mux.Unlock()
	var files []string
	if len(s.info.Files) == 0 {
		completed := true
		if !s.completed {
			for _, b := range s.pieces {
				if !b {
					completed = false
					break
				}
			}
		}
		if completed {
			files = append(files, s.info.Name)
		}
		return files
	}
	offset := 0
	for _, f := range s.info.Files {
		path := s.info.Name + "/" + strings.Join(f.Path, "/")
		completed := true
		startPiece := offset / int(s.info.PieceLength)
		endPiece := (offset + int(f.Length)) / int(s.info.PieceLength)
		offset += int(f.Length)
		if !s.completed && !s.completedFiles[path] {
			for i := startPiece; i <= endPiece; i++ {
				if i >= len(s.pieces) {
					break
				}
				if !s.pieces[i] {
					completed = false
					break
				}
			}
		}
		if completed {
			files = append(files, s.info.Name+"/"+strings.Join(f.Path, "/"))
			s.completedFiles[path] = true
		}
	}
	return files
}

// fileIndexes maps the paths this file speaks in (info.Name, then the file's
// own path) to positions in the torrent's file order. A single-file torrent is
// its one file, index 0.
func fileIndexes(info *metainfo.Info) map[string]int {
	if len(info.Files) == 0 {
		return map[string]int{info.Name: 0}
	}
	m := make(map[string]int, len(info.Files))
	for i, f := range info.Files {
		m[info.Name+"/"+strings.Join(f.Path, "/")] = i
	}
	return m
}

type pieceCompletion struct {
	mu sync.Mutex
	// events hears of a file becoming complete and ceasing to be (see
	// cache_events.go); nil publishes nothing. announced is what it has been
	// told and not yet taken back: the completion loop below reports every
	// complete file on every tick, and a consumer wants the transition.
	events      *CacheEvents
	fileIdx     map[string]int
	announced   map[string]bool
	closed      bool
	done        chan struct{}
	db          *sqlite.Conn
	info        *metainfo.Info
	hash        metainfo.Hash
	completions *completions
}

var _ storage.PieceCompletion = (*pieceCompletion)(nil)

func NewPieceCompletion(dir string, info *metainfo.Info, hash metainfo.Hash, events *CacheEvents) (ret *pieceCompletion, err error) {
	p := filepath.Join(dir, ".torrent.db")
	db, err := sqlite.OpenConn(p, 0)
	if err != nil {
		return
	}
	err = sqlitex.ExecScript(db, `create table if not exists piece_completion("index", complete, unique("index"))`)
	if err != nil {
		_ = db.Close()
		return
	}
	err = sqlitex.ExecScript(db, `create table if not exists file_completion("path", unique("path"))`)
	if err != nil {
		_ = db.Close()
		return
	}
	pieces := make([]bool, info.NumPieces())
	for i := 0; i < info.NumPieces(); i++ {
		pieces[i] = false
	}
	completedCount := 0
	err = sqlitex.Exec(db, `select "index", complete from piece_completion`,
		func(stmt *sqlite.Stmt) error {
			if stmt.ColumnInt(1) == 1 {
				index := stmt.ColumnInt(0)
				pieces[index] = true
				completedCount++
			}
			return nil
		},
	)
	if err != nil {
		_ = db.Close()
		return
	}
	completions := &completions{
		pieces:         pieces,
		completedCount: completedCount,
		completed:      completedCount == len(pieces),
		info:           info,
		completedFiles: make(map[string]bool),
	}
	ret = &pieceCompletion{
		db:          db,
		info:        info,
		hash:        hash,
		completions: completions,
		done:        make(chan struct{}),
		events:      events,
		fileIdx:     fileIndexes(info),
		announced:   make(map[string]bool),
	}
	go func() {
		// No local dedup map — always call CompleteFile() so that after
		// eviction + re-download the file_completion entry is re-added.
		// INSERT OR REPLACE is idempotent, so repeated calls are safe.
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			for _, f := range completions.GetCompletedFiles() {
				if err := ret.CompleteFile(f); err != nil {
					return
				}
			}
			if completions.isCompleted() {
				return
			}
			// Selectable sleep so Close() unblocks us immediately. The
			// previous bare `<-time.After(...)` left this goroutine
			// asleep through the full 5s after Close, and (when
			// Close() was never called at all — see mmap.Close prior
			// to fix) leaked it for the lifetime of the pod.
			select {
			case <-ret.done:
				return
			case <-ticker.C:
			}
		}
	}()
	return
}

func (s *pieceCompletion) Get(pk metainfo.PieceKey) (c storage.Completion, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = sqlitex.Exec(
		s.db, `select complete from piece_completion where "index"=?`,
		func(stmt *sqlite.Stmt) error {
			c.Complete = stmt.ColumnInt(0) != 0
			c.Ok = true
			return nil
		},
		pk.Index)
	return
}

func (s *pieceCompletion) Set(pk metainfo.PieceKey, b bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("closed")
	}
	if b {
		s.completions.Complete(pk.Index)
	} else {
		s.completions.Uncomplete(pk.Index)
	}
	return sqlitex.Exec(
		s.db,
		`insert or replace into piece_completion("index", complete) values(?, ?)`,
		nil,
		pk.Index,
		b,
	)
}

// UncompleteFiles removes entries from the file_completion table.
// Called during piece eviction to invalidate file-level cache.
func (s *pieceCompletion) UncompleteFiles(paths []string) error {
	gone, err := s.uncompleteFiles(paths)
	// Published after the lock is released, never under it: a publish can
	// block on a stalled socket, and this mutex is on the path of every
	// piece's Set.
	for _, idx := range gone {
		s.events.Uncached(s.hash.HexString(), idx)
	}
	return err
}

func (s *pieceCompletion) uncompleteFiles(paths []string) (gone []int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("closed")
	}
	for _, p := range paths {
		err := sqlitex.Exec(s.db, `delete from file_completion where "path"=?`, nil, p)
		if err != nil {
			return gone, err
		}
		// Only what was announced is taken back. A single-file torrent
		// larger than its cache budget lands here on EVERY evicted piece
		// without ever having been complete -- one event per piece per
		// stream, for nothing.
		if s.announced[p] {
			delete(s.announced, p)
			gone = append(gone, s.indexOf(p))
		}
	}
	return gone, nil
}

func (s *pieceCompletion) indexOf(path string) int {
	if i, ok := s.fileIdx[path]; ok {
		return i
	}
	return -1
}

func (s *pieceCompletion) CompleteFile(path string) error {
	announce, err := s.completeFile(path)
	if announce {
		s.events.Cached(s.hash.HexString(), s.indexOf(path))
	}
	return err
}

func (s *pieceCompletion) completeFile(path string) (announce bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, errors.New("closed")
	}
	err = sqlitex.Exec(
		s.db,
		`insert or replace into file_completion("path") values(?)`,
		nil,
		path,
	)
	// Once per completion, not once per tick. A torrent loaded again after a
	// restart or an unload announces its complete files anew: that is the
	// consumer's sign they are still here, and it costs one event per file
	// per load.
	if err == nil && !s.announced[path] {
		s.announced[path] = true
		announce = true
	}
	return announce, err
}

func (s *pieceCompletion) Close() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	close(s.done)
	err = s.db.Close()
	s.db = nil
	s.closed = true
	return
}
