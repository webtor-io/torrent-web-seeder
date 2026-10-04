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

// filePieces maps each file's path (as fileIndexes speaks it) to the first and
// last piece its bytes lie in; last < first for an empty file.
func filePieces(info *metainfo.Info) map[string][2]int {
	if len(info.Files) == 0 {
		return map[string][2]int{info.Name: {0, info.NumPieces() - 1}}
	}
	m := make(map[string][2]int, len(info.Files))
	var off int64
	for _, f := range info.Files {
		r := [2]int{int(off / info.PieceLength), int((off + f.Length - 1) / info.PieceLength)}
		if f.Length == 0 {
			r[1] = r[0] - 1
		}
		m[info.Name+"/"+strings.Join(f.Path, "/")] = r
		off += f.Length
	}
	return m
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
	pieces      map[string][2]int // see filePieces
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
	// The file's pieces, which the cache path checks in piece_completion
	// before it serves the file (FileCacheMap.Open). A row written before
	// these columns has them NULL and is not served from cache.
	for _, col := range []string{"first_piece", "last_piece"} {
		err = sqlitex.ExecScript(db, `alter table file_completion add column `+col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return
		}
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
		pieces:      filePieces(info),
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

// Get answers from the db alone: a piece with no row is known and not
// complete. Every hash result is written (MarkComplete / MarkNotComplete) and
// OpenTorrent creates the db's dir before opening it, so no row means the
// piece was never downloaded on this node. A failed query is not that: it
// stays Ok=false and returns its error.
//
// Ok=false means "unknown" to the library, and an unknown piece is hashed
// before it is ever requested (Torrent.queueInitialPieceCheck,
// Piece.ignoreForRequests). Answering unknown for a missing row made every
// add hash the whole never-downloaded torrent: a 93k-piece one held a pod at
// its 5-core limit for 10 minutes on 2026-09-23. With the initial check
// skipped as well (8f0cf39) nothing ever settled it, the pieces were never
// requested, and a torrent new to a pod stalled at 0 bytes: on 2026-09-24
// first-byte observations fell from 61-102 to 13-32 per 10 minutes and every
// self-hosted smoke scenario that reads from the seeder timed out.
func (s *pieceCompletion) Get(pk metainfo.PieceKey) (c storage.Completion, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = sqlitex.Exec(
		s.db, `select complete from piece_completion where "index"=?`,
		func(stmt *sqlite.Stmt) error {
			c.Complete = stmt.ColumnInt(0) != 0
			return nil
		},
		pk.Index)
	if err != nil {
		return storage.Completion{}, err
	}
	c.Ok = true
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
// Called during piece eviction to invalidate file-level cache. The files it
// takes back are published by publish, which the caller runs once it holds no
// lock: a publish can block on a stalled socket (nats.go writes synchronously
// once its buffer is full, for up to a minute). Not under this mutex, which is
// on the path of every piece's Set; not under eviction's exclusive hold of the
// torrent's dir, which every other pod's OpenTorrent waits for under its
// client lock.
func (s *pieceCompletion) UncompleteFiles(paths []string) (publish func(), err error) {
	gone, err := s.uncompleteFiles(paths)
	return func() {
		for _, idx := range gone {
			s.events.Uncached(s.hash.HexString(), idx)
		}
	}, err
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
	var first, last any // NULL for a path the torrent does not have
	if r, ok := s.pieces[path]; ok {
		first, last = r[0], r[1]
	}
	err = sqlitex.Exec(
		s.db,
		`insert or replace into file_completion("path", first_piece, last_piece) values(?, ?, ?)`,
		nil,
		path, first, last,
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
