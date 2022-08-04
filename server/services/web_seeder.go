package services

import (
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/anacrolix/torrent/bencode"

	"github.com/anacrolix/torrent"
	log "github.com/sirupsen/logrus"
)

var sha1R = regexp.MustCompile("^[0-9a-f]{5,40}$")

const (
	PIECE_PATH          = "piece/"
	SOURCE_TORRENT_PATH = "source.torrent"
	MAX_READAHEAD       = 250 * 1024 * 1024
	MIN_READAHEAD       = 1024 * 1024
)

type WebSeeder struct {
	tm *TorrentMap
	sm *SnapshotMap
	bp *BucketPool
	st *StatWeb
}

func NewWebSeeder(tm *TorrentMap, st *StatWeb, bp *BucketPool, sm *SnapshotMap) *WebSeeder {
	return &WebSeeder{
		tm: tm,
		bp: bp,
		st: st,
		sm: sm,
	}
}

func (s *WebSeeder) renderTorrent(w http.ResponseWriter, r *http.Request, h string) {
	log.Info("serve torrent")

	t, err := s.tm.Get(h)

	if err != nil {
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")

	err = bencode.NewEncoder(w).Encode(t.Metainfo())
	if err != nil {
		log.WithError(err).Error("failed to encode torrent")
		http.Error(w, "failed to encode torrent", http.StatusInternalServerError)
		return
	}
}

func (s *WebSeeder) renderPieceData(w http.ResponseWriter, r *http.Request, h string, ph string) {
	log.Infof("serve piece data for hash=%v piece hash=%v", h, ph)

	t, err := s.tm.Get(h)

	if err != nil {
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		w.WriteHeader(500)
		return
	}
	for i := 0; i < t.NumPieces(); i++ {
		p := t.Piece(i)
		if p.Info().Hash().HexString() == ph {
			pr := NewPieceReader(t.NewReader(), p)
			defer pr.Close()
			http.ServeContent(w, r, "", time.Unix(0, 0), pr)
		}
	}
}

func (s *WebSeeder) renderPiece(w http.ResponseWriter, r *http.Request, h string, ph string) {
	log.Infof("serve piece hash=%v piece-hash=%v", h, ph)
	if ph == "" {
		s.renderPieceIndex(w, r, h)
	} else {
		s.renderPieceData(w, r, h, ph)
	}
}

func (s *WebSeeder) renderPieceIndex(w http.ResponseWriter, r *http.Request, h string) {
	log.Info("serve piece index")

	t, err := s.tm.Get(h)

	if err != nil {
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
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

func (s *WebSeeder) renderTorrentIndex(w http.ResponseWriter, r *http.Request, h string) {
	log.Info("Serve file index")

	t, err := s.tm.Get(h)

	if err != nil {
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	s.addH(h, w, r)
	s.addA("..", w, r)
	s.addA(PIECE_PATH, w, r)
	s.addA(SOURCE_TORRENT_PATH, w, r)
	for _, f := range t.Files() {
		s.addA(f.Path(), w, r)
	}
}

func (s *WebSeeder) serveFile(w http.ResponseWriter, r *http.Request, h string, p string) {
	if _, ok := r.URL.Query()["stats"]; ok {
		s.serveStats(w, r, h, p)
		return
	}
	log := log.WithField("hash", h)
	log = log.WithField("path", r.URL.Path)
	log = log.WithField("method", r.Method)
	log = log.WithField("remoteAddr", r.RemoteAddr)
	found := false
	download := true
	keys, ok := r.URL.Query()["download"]
	if !ok || len(keys[0]) < 1 {
		download = false
	}
	log = log.WithField("download", download)

	t, err := s.tm.Get(h)

	if err != nil {
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	for _, f := range t.Files() {
		if f.Path() == p {
			log.WithField("range", r.Header.Get("Range")).Info("serve file")
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
			// torReader.SetResponsive()
			torReader.SetReadaheadFunc(func(r torrent.ReadaheadContext) int64 {
				p := f.Length() / 100
				if p < MIN_READAHEAD {
					p = MIN_READAHEAD
				}
				ra := (r.CurrentPos - r.ContiguousReadStartPos) * 2
				if ra < p {
					return p
				}
				if ra > MAX_READAHEAD {
					return MAX_READAHEAD
				}
				return ra
			})
			if r.Header.Get("X-Download-Rate") != "" && r.Header.Get("X-Session-ID") != "" {
				b, err := s.bp.Get(r.Header.Get("X-Session-ID"), r.Header.Get("X-Download-Rate"))
				if err != nil {
					log.WithError(err).Error("failed to get bucket")
					http.Error(w, "failed to get bucket", http.StatusInternalServerError)
					return
				}
				reader = NewThrottledReader(torReader, b)
			} else {
				reader = torReader
			}
			w.Header().Set("Last-Modified", time.Unix(0, 0).Format(http.TimeFormat))
			w.Header().Set("Etag", fmt.Sprintf("\"%x\"", sha1.Sum([]byte(t.InfoHash().String()+p))))
			w, err := s.sm.WrapWriter(w, h)
			if err != nil {
				http.Error(w, "failed to wrap writer", http.StatusInternalServerError)
				return
			}
			http.ServeContent(w, r, f.Path(), time.Unix(0, 0), reader)
			found = true
		}
	}
	if !found {
		log.Info("file not found")

		http.NotFound(w, r)
	}
}

func (s *WebSeeder) serveStats(w http.ResponseWriter, r *http.Request, h string, p string) {
	err := s.st.Serve(w, r, h, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *WebSeeder) getHash(r *http.Request) string {
	if r.Header.Get("X-Info-Hash") != "" {
		return r.Header.Get("X-Info-Hash")
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) > 0 {
		p := parts[0]
		f := sha1R.Find([]byte(p))
		if f != nil {
			return string(f)
		}
	}
	return ""
}

func (s *WebSeeder) renderIndex(w http.ResponseWriter, r *http.Request) {
	l, err := s.tm.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.addH("Index", w, r)
	for _, v := range l {
		s.addA(v+"/", w, r)
	}
}

func (s *WebSeeder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.getHash(r) == "" {
		s.renderIndex(w, r)
	} else {
		p := r.URL.Path[1:]
		p = strings.TrimPrefix(p, s.getHash(r)+"/")
		if p == "" {
			s.renderTorrentIndex(w, r, s.getHash(r))
		} else if p == SOURCE_TORRENT_PATH {
			s.renderTorrent(w, r, s.getHash(r))
		} else if strings.HasPrefix(p, PIECE_PATH) {
			tp := strings.TrimPrefix(p, PIECE_PATH)
			s.renderPiece(w, r, s.getHash(r), tp)
		} else {
			s.serveFile(w, r, s.getHash(r), p)
		}
	}
}
