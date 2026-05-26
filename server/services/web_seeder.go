package services

import (
	"context"
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

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

var sha1R = regexp.MustCompile("^[0-9a-f]{5,40}$")

const (
	SourceTorrentPath = "source.torrent"
	MaxReadaheadFlag  = "max-readahead"
)

func RegisterWebSeederFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   MaxReadaheadFlag,
			Usage:  "max readahead",
			Value:  "20MB",
			EnvVar: "MAX_READAHEAD",
		},
	)
}

type WebSeeder struct {
	tm           *TorrentMap
	st           *StatWeb
	wu           *Warmup
	fcm          *FileCacheMap
	tfcm         *TorrentFileCountMap
	tom          *TouchMap
	v            *Vault
	cl           *http.Client
	maxReadahead int64
}

func NewWebSeeder(tm *TorrentMap, fcm *FileCacheMap, tfcm *TorrentFileCountMap, tom *TouchMap, st *StatWeb, wu *Warmup, v *Vault, cl *http.Client, maxReadahead int64) *WebSeeder {
	return &WebSeeder{
		tm:           tm,
		st:           st,
		wu:           wu,
		fcm:          fcm,
		tfcm:         tfcm,
		tom:          tom,
		v:            v,
		cl:           cl,
		maxReadahead: maxReadahead,
	}
}

func (s *WebSeeder) renderTorrent(ctx context.Context, w http.ResponseWriter, h string) {
	log.Info("serve torrent")

	t, err := s.tm.Get(ctx, h)

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

	t, err := s.tm.Get(r.Context(), h)

	if err != nil {
		log.Error(err)
		http.Error(w, "failed to get torrent", http.StatusInternalServerError)
		return
	}
	s.addH(h, w)
	s.addA("..", w, r)
	s.addA(SourceTorrentPath, w, r)
	for _, f := range t.Info().UpvertedFiles() {
		s.addA(strings.Join(append([]string{t.Info().Name}, f.Path...), "/"), w, r)
	}
}

func (s *WebSeeder) serveFile(w http.ResponseWriter, r *http.Request, h string, p string) {
	_, download := r.URL.Query()["download"]

	logWithField := log.WithFields(log.Fields{
		"hash":       h,
		"path":       r.URL.Path,
		"method":     r.Method,
		"remoteAddr": r.RemoteAddr,
		"download":   download,
		"range":      r.Header.Get("Range"),
	})

	// Set common headers
	if download {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(p)+"\"")
	}
	if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	etag := fmt.Sprintf("\"%x\"", sha1.Sum([]byte(h+p)))
	lastMod := time.Unix(0, 0)

	// Handle conditional requests
	if match := r.Header.Get("If-None-Match"); match != "" {
		if match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !lastMod.After(t) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// Vault is the authoritative store (worker verifies piece SHA-1 before
	// committing to S3). The local file cache may contain stale bytes from
	// past piece evictions that punched holes into the mmap'd file before
	// the eviction race fix landed — file_completion can still report the
	// file as complete while the underlying bytes have zero-filled regions.
	// Serve from vault first; only fall back to the local cache when vault
	// does not have the file (typically while the resource is still being
	// ingested).
	if s.v != nil {
		served, err := s.redirectFromVault(w, r, h, p)
		if err != nil {
			logWithField.WithError(err).Warn("vault redirect failed, falling back to file cache or torrent")
		}
		if served {
			return
		}
	}

	// Vault does not have it — we will be serving from this seeder's local
	// torrent state (cache or live download). Touch the dir-level marker so
	// torrent-web-seeder-cleaner does not reap the torrent while it is in
	// use here. Skipped on the vault path above: redirects don't need this
	// seeder to keep the torrent loaded.
	if _, err := s.tom.Touch(h); err != nil {
		log.Error(err)
	}

	cp, err := s.fcm.Get(h, p)
	if err != nil {
		logWithField.WithError(err).Error("failed to check file cache")
		http.Error(w, "failed to check file cache", http.StatusInternalServerError)
		return
	}
	if cp != "" {
		logWithField.Info("serve file from cache")
		w.Header().Set("Last-Modified", lastMod.Format(http.TimeFormat))
		w.Header().Set("Etag", etag)
		file, err := os.Open(cp)
		if err != nil {
			logWithField.WithError(err).Error("failed to open cached file")
			http.Error(w, "failed to open cached file", http.StatusInternalServerError)
			return
		}
		defer file.Close()
		http.ServeContent(w, r, p, lastMod, file)
		return
	}

	// Fallback to torrent
	logWithField.Info("serve file from torrent")
	tw, reader, err := s.getTorrentReader(r.Context(), w, h, p)
	if err != nil {
		if strings.Contains(err.Error(), "PermissionDenied") {
			logWithField.WithError(err).Warn("permission denied")
			http.Error(w, "permission denied", http.StatusForbidden)
		} else if strings.Contains(err.Error(), "NotFound") {
			logWithField.WithError(err).Warn("not found")
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			logWithField.WithError(err).Error("failed to get torrent reader")
			http.Error(w, "failed to get reader", http.StatusInternalServerError)
		}
		return
	}
	if reader == nil {
		logWithField.Info("file not found")
		http.NotFound(w, r)
		return
	}
	defer reader.Close()

	tw.Header().Set("Last-Modified", lastMod.Format(http.TimeFormat))
	tw.Header().Set("Etag", etag)
	http.ServeContent(tw, r, p, lastMod, reader)
}

func (s *WebSeeder) redirectFromVault(w http.ResponseWriter, r *http.Request, h string, p string) (bool, error) {
	// Vault now serves individual files as soon as they're stored, even if
	// the resource isn't fully uploaded. Probe the file directly (cached)
	// instead of gating on whole-torrent availability.
	has, err := s.v.HasFile(r.Context(), h, p)
	if err != nil {
		return false, err
	}
	if !has {
		return false, nil
	}

	fileURL := s.v.FileURL(h, p)

	// Use a client that does not follow redirects so we can capture the Location header.
	noRedirectCl := *s.cl
	noRedirectCl.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// Preserve original method so vault can handle HEAD separately.
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(r.Context(), method, fileURL, nil)
	if err != nil {
		return false, err
	}

	resp, err := noRedirectCl.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}

	// GET: vault returns 302 with presigned S3 URL — pass it through.
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusTemporaryRedirect {
		loc := resp.Header.Get("Location")
		if loc != "" {
			http.Redirect(w, r, loc, resp.StatusCode)
			return true, nil
		}
	}

	// HEAD: vault returns 200 with Content-Length from S3 HeadObject.
	if resp.StatusCode == http.StatusOK {
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			w.Header().Set("Content-Length", cl)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return true, nil
	}

	return false, fmt.Errorf("unexpected vault status %d for %s", resp.StatusCode, fileURL)
}

func (s *WebSeeder) getTorrentReader(ctx context.Context, w http.ResponseWriter, h string, p string) (http.ResponseWriter, io.ReadSeekCloser, error) {
	t, err := s.tm.Get(ctx, h)
	if err != nil {
		return w, nil, err
	}

	for _, f := range t.Files() {
		if f.Path() == p {
			torReader := f.NewReader()
			torReader.SetResponsive()
			torReader.SetReadaheadFunc(NewReadaheadFunc(s.maxReadahead))
			return NewTouchWriter(w, s.tm, h), torReader, nil
		}
	}
	return w, nil, nil
}

// availableWithoutTorrent checks if the file/directory/root is available via cache or vault,
// meaning no torrent download is needed.
func (s *WebSeeder) availableWithoutTorrent(ctx context.Context, h string, p string) (bool, error) {
	if p == "" {
		// Root: check if all torrent files are cached
		totalFiles, err := s.tfcm.TotalFiles(h)
		if err != nil {
			log.WithError(err).Warnf("failed to get total files for %s", h)
		} else if totalFiles > 0 {
			complete, err := s.fcm.IsDirComplete(h, "", totalFiles)
			if err != nil {
				log.WithError(err).Warnf("failed to check dir complete for %s", h)
			} else if complete {
				return true, nil
			}
		}
	} else {
		// Try exact file match first
		cp, err := s.fcm.Get(h, p)
		if err != nil {
			return false, err
		}
		if cp != "" {
			return true, nil
		}
		// Try as directory prefix
		dirFiles, err := s.tfcm.DirFileCount(h, p)
		if err != nil {
			log.WithError(err).Warnf("failed to get dir file count for %s/%s", h, p)
		} else if dirFiles > 0 {
			complete, err := s.fcm.IsDirComplete(h, p, dirFiles)
			if err != nil {
				log.WithError(err).Warnf("failed to check dir complete for %s/%s", h, p)
			} else if complete {
				return true, nil
			}
		}
	}
	// Check vault
	if s.v != nil {
		wsURL, err := s.v.GetWebseedURL(ctx, h)
		if err != nil {
			log.WithError(err).Warnf("failed to check vault for %s", h)
		} else if wsURL != "" {
			return true, nil
		}
		// Whole torrent isn't stored, but a specific file may be —
		// vault serves per-file as soon as the individual file is stored.
		if p != "" {
			has, err := s.v.HasFile(ctx, h, p)
			if err != nil {
				log.WithError(err).Warnf("failed to check vault file for %s/%s", h, p)
			} else if has {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *WebSeeder) serveStats(w http.ResponseWriter, r *http.Request, h string, p string) {
	avail, err := s.availableWithoutTorrent(r.Context(), h, p)
	if err != nil {
		log.Error(err)
		http.Error(w, "failed to check availability", http.StatusInternalServerError)
		return
	}
	if avail {
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

func (s *WebSeeder) serveWarmup(w http.ResponseWriter, r *http.Request, h string, p string) {
	avail, err := s.availableWithoutTorrent(r.Context(), h, p)
	if err != nil {
		log.Error(err)
		http.Error(w, "failed to check availability", http.StatusInternalServerError)
		return
	}
	// File is already in vault/cache — there is nothing to warm. Return
	// an empty SSE stream; the closed connection is the "done" signal
	// the client checks for.
	if avail {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	// Keep the torrent alive while we're warming it — same reason as
	// serveFile: torrent-web-seeder-cleaner will reap it otherwise.
	if _, err := s.tom.Touch(h); err != nil {
		log.Error(err)
	}
	err = s.wu.Serve(w, r, h, p)
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
		if _, ok := r.URL.Query()["stats"]; ok {
			s.serveStats(w, r, h, p)
		} else if _, ok := r.URL.Query()["warmup"]; ok {
			s.serveWarmup(w, r, h, p)
		} else if _, ok := r.URL.Query()["done"]; ok {
			s.serveDone(w, r, h, p)
		} else if p == "" {
			s.renderTorrentIndex(w, r, h)
		} else if p == SourceTorrentPath {
			s.renderTorrent(r.Context(), w, h)
		} else {
			s.serveFile(w, r, h, p)
		}
	}
}

func (s *WebSeeder) serveDone(w http.ResponseWriter, r *http.Request, h string, p string) {
	avail, err := s.availableWithoutTorrent(r.Context(), h, p)
	if err != nil {
		log.Error(err)
		http.Error(w, "failed to check availability", http.StatusInternalServerError)
		return
	}
	if avail {
		return
	}
	http.NotFound(w, r)
}

func NewReadaheadFunc(maxReadahead int64) torrent.ReadaheadFunc {
	return func(_ torrent.ReadaheadContext) int64 {
		return maxReadahead
	}
}
