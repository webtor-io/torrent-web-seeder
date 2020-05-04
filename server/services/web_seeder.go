package services

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"io"

	"code.cloudfoundry.org/bytefmt"
	log "github.com/sirupsen/logrus"

	"github.com/anacrolix/torrent/bencode"
)

const (
	PIECE_PATH          = "piece/"
	SOURCE_TORRENT_PATH = "source.torrent"
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

	s.addH(fmt.Sprintf("%s - pieces", t.InfoHash().HexString()), w, r)
	s.addA("../", w, r)
	for i := 0; i < t.NumPieces(); i++ {
		p := t.Piece(i)
		h := p.Info().Hash().HexString()
		s.addA(h, w, r)
	}
}
func (s *WebSeeder) addA(path string, w http.ResponseWriter, r *http.Request) {
	uHref := url.URL{
		Path:     path,
		RawQuery: r.URL.RawQuery,
	}
	uName := url.URL{
		Path: path,
	}
	href := uHref.String()
	name := uName.String()
	fmt.Fprintln(w, fmt.Sprintf("<a href=\"%s\">%s</a><br />", href, name))
}
func (s *WebSeeder) addH(h string, w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, fmt.Sprintf("<h1>%s</h1>", h))
}
func (s *WebSeeder) renderIndex(w http.ResponseWriter, r *http.Request) {
	log.Info("Serve file index")

	t, err := s.t.Get()

	if err != nil {
		http.Error(w, "Failed to get torrent", http.StatusInternalServerError)
		return
	}
	h := t.InfoHash().HexString()
	s.addH(h, w, r)
	s.addA("/"+h+"/", w, r)
	s.addA(PIECE_PATH, w, r)
	s.addA(SOURCE_TORRENT_PATH, w, r)
	for _, f := range t.Files() {
		s.addA(f.Path(), w, r)
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
			torReader.SetReadahead(15 * 1024 * 1024)
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
	} else if p == SOURCE_TORRENT_PATH {
		s.renderTorrent(w, r)
	} else if strings.HasPrefix(p, PIECE_PATH) {
		h := strings.TrimPrefix(p, PIECE_PATH)
		s.renderPiece(w, r, h)
	} else {
		s.serveFile(w, r, p)
	}
}
