package services

import (
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"io"

	"code.cloudfoundry.org/bytefmt"
	log "github.com/sirupsen/logrus"
)

type WebSeeder struct {
	t   *Torrent
	err error
}

func NewWebSeeder(t *Torrent) *WebSeeder {
	return &WebSeeder{t: t}
}

func (s *WebSeeder) renderIndex(w http.ResponseWriter, r *http.Request) {
	log.Info("Serve index")

	t, err := s.t.Get()

	if err != nil {
		http.Error(w, "Failed to get torrent", http.StatusInternalServerError)
		return
	}

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
			if r.Header.Get("X-Download-Rate") != "" {
				rate, err := bytefmt.ToBytes(r.Header.Get("X-Download-Rate"))
				if err != nil {
					log.WithError(err).Error("Wrong download rate")
					http.Error(w, "Wrong download rate", http.StatusInternalServerError)
					return
				}
				reader = NewThrottledReader(f.NewReader(), rate)
			} else {
				reader = f.NewReader()

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
	p := r.URL.Path[1:]
	if p == "" {
		s.renderIndex(w, r)
	} else {
		s.serveFile(w, r, p)
	}
}
