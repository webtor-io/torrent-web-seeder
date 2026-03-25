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

type pieceCompletion struct {
	mu          sync.Mutex
	closed      bool
	db          *sqlite.Conn
	info        *metainfo.Info
	hash        metainfo.Hash
	completions *completions
}

var _ storage.PieceCompletion = (*pieceCompletion)(nil)

func NewPieceCompletion(dir string, info *metainfo.Info, hash metainfo.Hash) (ret *pieceCompletion, err error) {
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
	}
	go func() {
		// No local dedup map — always call CompleteFile() so that after
		// eviction + re-download the file_completion entry is re-added.
		// INSERT OR REPLACE is idempotent, so repeated calls are safe.
		for {
			for _, f := range completions.GetCompletedFiles() {
				err = ret.CompleteFile(f)
				if err != nil {
					return
				}
			}
			if completions.completed || ret.closed {
				return
			}
			<-time.After(5 * time.Second)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("closed")
	}
	for _, p := range paths {
		err := sqlitex.Exec(s.db, `delete from file_completion where "path"=?`, nil, p)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *pieceCompletion) CompleteFile(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("closed")
	}
	return sqlitex.Exec(
		s.db,
		`insert or replace into file_completion("path") values(?)`,
		nil,
		path,
	)
}

func (s *pieceCompletion) Close() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	err = s.db.Close()
	s.db = nil
	s.closed = true
	return
}
