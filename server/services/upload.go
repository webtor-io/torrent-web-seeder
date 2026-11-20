package services

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const APISecretFlag = "api-secret"

func RegisterUploadFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   APISecretFlag,
			Usage:  "secret the API signs tokens with; uploads are accepted only with it",
			EnvVar: "API_SECRET",
		},
	)
}

// Upload serves the IDs whose content lives on their home node
// (uploads.HomeHosted) from their dir, GetDir(DATA_DIR, id): the same dir a
// torrent with that ID would have, next to the torrents' cache.
//
// The dir's layout: files/ holds the files, whole by construction; parts/
// the parts being written; owner the writer's uid. Its .lock is created only
// when the upload is, and whoever uses the dir holds .lock shared, which keeps
// the cleaner (it takes .lock exclusive, without waiting) off it.
type Upload struct {
	ws     *WebSeeder
	secret []byte
}

// WithUploads turns the branch on: without the API secret no write token can
// be checked, and the class goes the torrent way.
func (s *WebSeeder) WithUploads(secret string) *WebSeeder {
	if secret == "" {
		log.Info("uploads: off, API secret is not set")
		return s
	}
	s.up = &Upload{ws: s, secret: []byte(secret)}
	log.Info("uploads: on")
	return s
}

// ServeHTTP serves a request for upload h; p is the path inside it.
func (u *Upload) ServeHTTP(w http.ResponseWriter, r *http.Request, h, p string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		u.read(w, r, h, p)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// read answers the torrent queries the way a torrent fully cached here does
// and serves the files. The queries come before the path, as in
// WebSeeder.ServeHTTP; none of them touches the dir.
func (u *Upload) read(w http.ResponseWriter, r *http.Request, h, p string) {
	switch q := r.URL.Query(); {
	case q.Has("stats"):
		http.NotFound(w, r) // nothing to download: "in the cache"
	case q.Has("warmup"):
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK) // an empty stream: nothing to warm
	case q.Has("done"): // 200: done
	case p == "":
		http.NotFound(w, r)
	case p == SourceTorrentPath:
		http.NotFound(w, r)
	default:
		u.serveFile(w, r, h, p)
	}
}

// serveFile serves p from the dir, else from Vault, else 404. Local bytes come
// first, the reverse of a torrent's order: the node's disk is cheaper than
// S3, and Vault has the files once the dir is evicted. Vault's own reads get
// the local bytes only.
//
// The dir is touched only once the file is open: touching an evicted or
// deleted upload would create an orphan <id>.touch.
func (u *Upload) serveFile(w http.ResponseWriter, r *http.Request, h, p string) {
	logger := log.WithFields(log.Fields{
		"hash":       h,
		"path":       r.URL.Path,
		"method":     r.Method,
		"remoteAddr": r.RemoteAddr,
	})
	dir, err := GetDir(u.ws.tom.p, h)
	if err != nil {
		logger.WithError(err).Error("upload read failed")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f, release, err := openUploadFile(dir, p)
	if err != nil {
		logger.WithError(err).Error("upload read failed")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if f != nil {
		defer release()
		if _, err := u.ws.tom.Touch(h); err != nil {
			logger.WithError(err).Error("upload touch failed")
		}
		serveHeaders(w, r, p)
		etag := fmt.Sprintf("\"%x\"", sha1.Sum([]byte(h+p)))
		// As on the torrent cache path: a client that stops reading would
		// hold .lock, and so the dir, from the cleaner for as long as it
		// stays connected.
		tw := NewTouchWriter(w, nil, u.ws.tom, h, h)
		go watchStall(r.Context(), func() { release(); abortWrite(r) }, tw.LastWrite, u.ws.stallTimeout, logger)
		serveWithValidators(tw, r, p, time.Unix(0, 0), etag, f)
		return
	}
	if u.ws.v != nil && r.Header.Get("X-Role") != "vault" {
		served, err := u.ws.redirectFromVault(w, r, h, p)
		if err != nil {
			logger.WithError(err).Warn("vault redirect failed")
		}
		if served {
			return
		}
	}
	http.NotFound(w, r)
}

// errUploadGone: the upload has no dir here, or the cleaner removed it.
var errUploadGone = errors.New("upload is gone")

// lockUpload takes dir's .lock shared. .lock is not created: a dir without
// one is gone, as is one whose .lock was unlinked while this waited for the
// cleaner's exclusive hold to end (Nlink 0).
func lockUpload(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDONLY, 0)
	if os.IsNotExist(err) {
		return nil, errUploadGone
	}
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		_ = f.Close()
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Nlink == 0 { // uint16 on darwin, uint64 on linux
		_ = f.Close()
		return nil, errUploadGone
	}
	return f, nil
}

// uploadFile is p's path in dir's files/ and whether it stays inside it. A
// read path is not validated, so this is its only guard.
func uploadFile(dir, p string) (string, bool) {
	root := filepath.Join(dir, "files")
	full := filepath.Join(root, p)
	return full, strings.HasPrefix(full, root+string(filepath.Separator))
}

// openUploadFile opens dir's file p under the dir's shared .lock; release
// closes both. A path that is not a regular file of files/ is a miss: nil,
// nil, nil.
func openUploadFile(dir, p string) (*os.File, func(), error) {
	lock, err := lockUpload(dir)
	if errors.Is(err, errUploadGone) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	full, ok := uploadFile(dir, p)
	if !ok {
		_ = lock.Close()
		return nil, nil, nil
	}
	f, err := os.Open(full)
	if err != nil {
		_ = lock.Close()
		// The last two: a segment over 255 bytes or a NUL, which any holder
		// of a read token can send.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) ||
			errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, syscall.EINVAL) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		_ = lock.Close()
		return nil, nil, err
	}
	var once sync.Once
	return f, func() {
		once.Do(func() {
			_ = f.Close()
			_ = lock.Close()
		})
	}, nil
}
