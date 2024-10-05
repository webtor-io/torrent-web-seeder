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
	PiecePath         = "piece/"
	SourceTorrentPath = "source.torrent"
	MaxReadahead      = 250 * 1024 * 1024
	MinReadahead      = 1024 * 1024
)

type WebSeeder struct {
	tm *TorrentMap
	st *StatWeb
}

func NewWebSeeder(tm *TorrentMap, st *StatWeb) *WebSeeder {
	return &WebSeeder{
		tm: tm,
		st: st,
	}
}

func (s *WebSeeder) renderTorrent(w http.ResponseWriter, h string) {
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
	_, _ = fmt.Fprintln(w, fmt.Sprintf("<a href=\"%s\">%s</a><br />", href, name))
}
func (s *WebSeeder) addH(h string, w http.ResponseWriter) {
	_, _ = fmt.Fprintln(w, fmt.Sprintf("<h1>%s</h1>", h))
}

func (s *WebSeeder) renderTorrentIndex(w http.ResponseWriter, r *http.Request, h string) {
	log.Info("Serve file index")

	t, err := s.tm.Get(h)

	if err != nil {
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	s.addH(h, w)
	s.addA("..", w, r)
	s.addA(PiecePath, w, r)
	s.addA(SourceTorrentPath, w, r)
	for _, f := range t.Files() {
		s.addA(f.Path(), w, r)
	}
}

func (s *WebSeeder) serveFile(w http.ResponseWriter, r *http.Request, h string, p string) {
	if _, ok := r.URL.Query()["stats"]; ok {
		s.serveStats(w, r, h, p)
		return
	}
	logWIthField := log.WithField("hash", h)
	logWIthField = logWIthField.WithField("path", r.URL.Path)
	logWIthField = logWIthField.WithField("method", r.Method)
	logWIthField = logWIthField.WithField("remoteAddr", r.RemoteAddr)
	found := false
	download := true
	keys, ok := r.URL.Query()["download"]
	if !ok || len(keys[0]) < 1 {
		download = false
	}
	logWIthField = logWIthField.WithField("download", download)

	t, err := s.tm.Get(h)

	if err != nil {
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	for _, f := range t.Files() {
		if f.Path() == p {
			logWIthField.WithField("range", r.Header.Get("Range")).Info("serve file")
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
				if p < MinReadahead {
					p = MinReadahead
				}
				ra := (r.CurrentPos - r.ContiguousReadStartPos) * 2
				if ra < p {
					return p
				}
				if ra > MaxReadahead {
					return MaxReadahead
				}
				return ra
			})
			reader = torReader
			w.Header().Set("Last-Modified", time.Unix(0, 0).Format(http.TimeFormat))
			w.Header().Set("Etag", fmt.Sprintf("\"%x\"", sha1.Sum([]byte(t.InfoHash().String()+p))))
			http.ServeContent(w, r, f.Path(), time.Unix(0, 0), reader)
			found = true
		}
	}
	if !found {
		logWIthField.Info("file not found")

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
	s.addH("Index", w)
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
		} else if p == SourceTorrentPath {
			s.renderTorrent(w, s.getHash(r))
		} else {
			s.serveFile(w, r, s.getHash(r), p)
		}
	}
}
