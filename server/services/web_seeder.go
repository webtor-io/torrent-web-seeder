package services

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"io"

	"code.cloudfoundry.org/bytefmt"
	log "github.com/sirupsen/logrus"

	"github.com/anacrolix/torrent/bencode"
)

type WebSeeder struct {
	t   *Torrent
	err error
}

func NewWebSeeder(t *Torrent) *WebSeeder {
	return &WebSeeder{t: t}
}

func (s *WebSeeder) renderTorrent(w http.ResponseWriter, r *http.Request) {
	log.Info("Serve torrent")

	t, err := s.t.Get()

	if err != nil {
		http.Error(w, "Failed to get torrent", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")

	err = bencode.NewEncoder(w).Encode(t.Metainfo())
	if err != nil {
		log.WithError(err).Error("Failed to encode torrent")
		http.Error(w, "Failed to encode torrent", http.StatusInternalServerError)
		return
	}
}

func (s *WebSeeder) renderPieceData(w http.ResponseWriter, r *http.Request, hash string) {
	log.Infof("Serve piece data for hash=%v", hash)

	t, err := s.t.Get()

	if err != nil {
		http.Error(w, "Failed to get torrent", http.StatusInternalServerError)
		w.WriteHeader(500)
		return
	}
	for i := 0; i < t.NumPieces(); i++ {
		p := t.Piece(i)
		if p.Info().Hash().HexString() == hash {
			tr := t.NewReader()
			defer tr.Close()
			tr.Seek(p.Info().Offset(), io.SeekStart)
			lr := io.LimitReader(tr, p.Info().Length())
			_, err := io.Copy(w, lr)
			if err != nil && err != io.EOF {
				log.WithError(err).Error("Failed to read piece data")
				w.WriteHeader(500)
				return
			}
			return
		}
	}
}

func (s *WebSeeder) renderPiece(w http.ResponseWriter, r *http.Request, hash string) {
	log.Infof("Serve piece hash=%v", hash)
	if hash == "" {
		s.renderPieceIndex(w, r)
	} else {
		s.renderPieceData(w, r, hash)
	}
}

func (s *WebSeeder) renderPieceIndex(w http.ResponseWriter, r *http.Request) {
	log.Info("Serve piece index")

	t, err := s.t.Get()

	if err != nil {
		http.Error(w, "Failed to get torrent", http.StatusInternalServerError)
		return
	}

	fmt.Fprintln(w, fmt.Sprintf("<h1>%s - pieces</h1>", t.InfoHash().HexString()))
	fmt.Fprintln(w, "<a href=\"../\">..</a><br />")
	for i := 0; i < t.NumPieces(); i++ {
		p := t.Piece(i)
		h := p.Info().Hash().HexString()
		fmt.Fprintln(w, fmt.Sprintf("<a href=\"%s\">%s</a><br />", h, h))
	}
}
func (s *WebSeeder) renderIndex(w http.ResponseWriter, r *http.Request) {
	log.Info("Serve file index")

	t, err := s.t.Get()

	if err != nil {
		http.Error(w, "Failed to get torrent", http.StatusInternalServerError)
		return
	}
	h := t.InfoHash().HexString()

	fmt.Fprintln(w, fmt.Sprintf("<h1>%s</h1>", h))
	fmt.Fprintln(w, fmt.Sprintf("<a href=\"/%s/\">/%s/</a><br />", h, h))
	fmt.Fprintln(w, "<a href=\".piece/\">.piece/</a><br />")
	fmt.Fprintln(w, "<a href=\".source.torrent\">.source.torrent</a><br />")
	for _, f := range t.Files() {
		fmt.Fprintln(w, fmt.Sprintf("<a href=\"%s\">%s</a><br />", f.Path(), f.Path()))
	}
}

func (s *WebSeeder) serveFile(w http.ResponseWriter, r *http.Request, p string) {
	log := log.WithField("path", r.URL.Path)
	log = log.WithField("method", r.Method)
	log = log.WithField("remoteAddr", r.RemoteAddr)
	found := false
	download := true
	keys, ok := r.URL.Query()["download"]
	if !ok || len(keys[0]) < 1 {
		download = false
	}
	log = log.WithField("download", download)

	t, err := s.t.Get()

	if err != nil {
		http.Error(w, "Failed to get torrent", http.StatusInternalServerError)
		return
	}
	for _, f := range t.Files() {
		if f.Path() == p {
			log.WithField("range", r.Header.Get("Range")).Info("Serve file")
			if download {
				w.Header().Add("Content-Type", "application/octet-stream")
				w.Header().Add("Content-Disposition", "attachment; filename=\""+filepath.Base(p)+"\"")
			}
			if r.Header.Get("Origin") != "" {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			var reader io.ReadSeeker
			torReader := f.NewReader()
			torReader.SetResponsive()
			torReader.SetReadahead(50 * 1024 * 1024)
			if r.Header.Get("X-Download-Rate") != "" {
				rate, err := bytefmt.ToBytes(r.Header.Get("X-Download-Rate"))
				if err != nil {
					log.WithError(err).Error("Wrong download rate")
					http.Error(w, "Wrong download rate", http.StatusInternalServerError)
					return
				}
				reader = NewThrottledReader(torReader, rate)
			} else {
				reader = torReader
			}
			http.ServeContent(w, r, f.Path(), time.Unix(t.Metainfo().CreationDate, 0), reader)
			found = true
		}
	}
	if !found {
		log.Info("File not found")

		http.NotFound(w, r)
	}
}

func (s *WebSeeder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t, err := s.t.Get()
	if err != nil {
		log.WithError(err).Error("Failed to get torrent")
		w.WriteHeader(500)
		return
	}

	p := r.URL.Path[1:]
	p = strings.TrimPrefix(p, t.InfoHash().HexString()+"/")
	if p == "" {
		s.renderIndex(w, r)
	} else if p == ".source.torrent" {
		s.renderTorrent(w, r)
	} else if strings.HasPrefix(p, ".piece/") {
		h := strings.TrimPrefix(p, ".piece/")
		s.renderPiece(w, r, h)
	} else {
		s.serveFile(w, r, p)
	}
}
