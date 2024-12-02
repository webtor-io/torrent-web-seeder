package services

import (
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/anacrolix/torrent/bencode"

	log "github.com/sirupsen/logrus"
)

var sha1R = regexp.MustCompile("^[0-9a-f]{5,40}$")

const (
	SourceTorrentPath = "source.torrent"
)

type WebSeeder struct {
	tm  *TorrentMap
	st  *StatWeb
	fcm *FileCacheMap
	tom *TouchMap
}

func NewWebSeeder(tm *TorrentMap, fcm *FileCacheMap, tom *TouchMap, st *StatWeb) *WebSeeder {
	return &WebSeeder{
		tm:  tm,
		st:  st,
		fcm: fcm,
		tom: tom,
	}
}

func (s *WebSeeder) renderTorrent(w http.ResponseWriter, h string) {
	log.Info("serve torrent")

	t, err := s.tm.Get(h)

	if err != nil {
		log.Error(err)
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
		log.Error(err)
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	s.addH(h, w)
	s.addA("..", w, r)
	s.addA(SourceTorrentPath, w, r)
	for _, f := range t.Files() {
		s.addA(f.Path(), w, r)
	}
}

func (s *WebSeeder) serveFile(w http.ResponseWriter, r *http.Request, h string, p string) {
	err := s.tom.Touch(h)
	if err != nil {
		log.Error(err)
	}

	_, download := r.URL.Query()["download"]

	logWIthField := log.WithFields(log.Fields{
		"hash":       h,
		"path":       r.URL.Path,
		"method":     r.Method,
		"remoteAddr": r.RemoteAddr,
		"download":   download,
		"range":      r.Header.Get("Range"),
	})

	w, reader, err := s.getReader(w, h, p)
	if err != nil {
		log.Error(err)
		http.Error(w, "failed to get reader", http.StatusInternalServerError)
		return
	}
	if reader == nil {
		logWIthField.Info("file not found")
		http.NotFound(w, r)
		return
	}
	defer func(reader io.ReadSeekCloser) {
		_ = reader.Close()
	}(reader)

	logWIthField.Info("serve file")
	if download {
		w.Header().Add("Content-Type", "application/octet-stream")
		w.Header().Add("Content-Disposition", "attachment; filename=\""+filepath.Base(p)+"\"")
	}
	if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Last-Modified", time.Unix(0, 0).Format(http.TimeFormat))
	w.Header().Set("Etag", fmt.Sprintf("\"%x\"", sha1.Sum([]byte(h+p))))
	http.ServeContent(w, r, p, time.Unix(0, 0), reader)
}

func (s *WebSeeder) getReader(w http.ResponseWriter, h string, p string) (http.ResponseWriter, io.ReadSeekCloser, error) {
	cp, err := s.fcm.Get(h, p)
	if err != nil {
		return nil, nil, err
	}

	if cp != "" {
		return s.openCachedFile(w, cp)
	}

	return s.getTorrentReader(w, h, p)
}

func (s *WebSeeder) openCachedFile(w http.ResponseWriter, cp string) (http.ResponseWriter, io.ReadSeekCloser, error) {
	file, err := os.Open(cp)
	if err != nil {
		return w, nil, err
	}
	return w, file, nil
}

func (s *WebSeeder) getTorrentReader(w http.ResponseWriter, h string, p string) (http.ResponseWriter, io.ReadSeekCloser, error) {
	t, err := s.tm.Get(h)
	if err != nil {
		return w, nil, err
	}

	for _, f := range t.Files() {
		if f.Path() == p {
			torReader := f.NewReader()
			torReader.SetResponsive()
			return NewTouchWriter(w, s.tm, h), torReader, nil
		}
	}
	return w, nil, nil
}

func (s *WebSeeder) serveStats(w http.ResponseWriter, r *http.Request, h string, p string) {
	cp, err := s.fcm.Get(h, p)
	if err != nil {
		log.Error(err)
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	if cp != "" {
		http.NotFound(w, r)
		return
	}
	err = s.st.Serve(w, r, h, p)
	if err != nil {
		log.Error(err)
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
		log.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.addH("Index", w)
	for _, v := range l {
		s.addA(v+"/", w, r)
	}
}

func (s *WebSeeder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := s.getHash(r)
	if h == "" {
		s.renderIndex(w, r)
	} else {
		p := r.URL.Path[1:]
		p = strings.TrimPrefix(p, h+"/")
		if p == "" {
			s.renderTorrentIndex(w, r, h)
		} else if p == SourceTorrentPath {
			s.renderTorrent(w, s.getHash(r))
		} else if _, ok := r.URL.Query()["stats"]; ok {
			s.serveStats(w, r, h, p)
		} else if _, ok := r.URL.Query()["done"]; ok {
			s.serveDone(w, r, h, p)
		} else {
			s.serveFile(w, r, s.getHash(r), p)
		}
	}
}

func (s *WebSeeder) serveDone(w http.ResponseWriter, r *http.Request, h string, p string) {
	cp, err := s.fcm.Get(h, p)
	if err != nil {
		log.Error(err)
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	if cp != "" {
		return
	} else {
		http.NotFound(w, r)
	}
}
