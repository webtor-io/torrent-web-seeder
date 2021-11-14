package services

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"io"

	log "github.com/sirupsen/logrus"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
)

const (
	PIECE_PATH          = "piece/"
	SOURCE_TORRENT_PATH = "source.torrent"
	MAX_READAHEAD       = 50 * 1024 * 1024
)

type WebSeeder struct {
	t   *Torrent
	c   *Counter
	bp  *BucketPool
	err error
}

func NewWebSeeder(t *Torrent, c *Counter, bp *BucketPool) *WebSeeder {
	return &WebSeeder{
		t:  t,
		c:  c,
		bp: bp,
	}
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
			pr := NewPieceReader(t.NewReader(), p)
			defer pr.Close()
			http.ServeContent(w, r, "", time.Unix(0, 0), pr)
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
			torReader.SetReadaheadFunc(func(r torrent.ReadaheadContext) int64 {
				ra := (r.CurrentPos-r.ContiguousReadStartPos)*2 + 1024*1024
				if ra > MAX_READAHEAD {
					return MAX_READAHEAD
				}
				return ra
			})
			if r.Header.Get("X-Download-Rate") != "" && r.Header.Get("X-Session-ID") != "" {
				b, err := s.bp.Get(r.Header.Get("X-Session-ID"), r.Header.Get("X-Download-Rate"))
				if err != nil {
					log.WithError(err).Error("Failed to get bucket")
					http.Error(w, "Failed to get bucket", http.StatusInternalServerError)
					return
				}
				reader = NewThrottledReader(torReader, b)
			} else {
				reader = torReader
			}
			w.Header().Set("Last-Modified", time.Unix(0, 0).Format(http.TimeFormat))
			w.Header().Set("Etag", fmt.Sprintf("\"%x\"", sha1.Sum([]byte(t.InfoHash().String()+p))))
			http.ServeContent(s.c.NewResponseWriter(w), r, f.Path(), time.Unix(0, 0), reader)
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
