package services

import (
	"context"
	"crypto/sha1"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli"
)

// IDs of the uploads tests: the shared vectors of each kind.
const (
	upID     = "9c5db5752b7c0123456789abcdef0123456789ab" // kind 0
	upID1    = "9c5db5752b7c1123456789abcdef0123456789ab" // kind 1
	upIDf    = "9c5db5752b7cf123456789abcdef0123456789ab" // kind f
	upSecret = "test-api-secret"
)

// upSeeder is a WebSeeder over a fresh DATA_DIR with uploads on (upSecret),
// stallTimeout 1 min, no tm/fcm/tfcm/v: a request that leaves the branch for
// the torrent code panics.
func upSeeder(t *testing.T) (ws *WebSeeder, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	ws = (&WebSeeder{tom: testTouchMap(t, dataDir), stallTimeout: time.Minute}).WithUploads(upSecret)
	return ws, dataDir
}

// upDir lays id's dir out as up=new leaves it: <id>.touch, files/, parts/,
// owner = uidA, .lock; files maps "Name/rel" to content. Returns the dir.
func upDir(t *testing.T, dataDir, id string, files map[string]string) string {
	t.Helper()
	dir, err := GetDir(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+".touch", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"files", "parts"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(uidA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(dir, "files", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// cleanerDrop is the cleaner's drop (torrent-web-seeder-cleaner 0965d3f,
// services/cleaner.go:146-170) without the eviction gate: .lock opened
// O_RDONLY|O_CREATE and taken LOCK_EX|LOCK_NB (busy: false), the dir removed,
// then its .touch.
func cleanerDrop(t *testing.T, dir string) bool {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDONLY|os.O_CREATE, 0o644)
	if err == nil {
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
			return false
		} else if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) { // not there: only the .touch is left
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir + ".touch"); err != nil {
		t.Fatal(err)
	}
	return true
}

// serve runs r through ws and cancels r's context once the handler returns,
// as net/http does: that ends the response's watchStall.
func serve(ws *WebSeeder, r *http.Request) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	rec := httptest.NewRecorder()
	ws.ServeHTTP(rec, r.WithContext(ctx))
	return rec
}

// upVault starts a fake Vault answering with h, sets ws.cl = http.DefaultClient
// and ws.v = NewVault(vault-host/port of it), counts its requests.
func upVault(t *testing.T, ws *WebSeeder, h http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().(*net.TCPAddr)
	fs := flag.NewFlagSet("vault", flag.ContinueOnError)
	fs.String(vaultHostFlag, addr.IP.String(), "")
	fs.Int(vaultPortFlag, addr.Port, "")
	ws.cl = http.DefaultClient
	ws.v = NewVault(cli.NewContext(nil, fs, nil), ws.cl)
	return &n
}

// ageTouch sets <dir>.touch's mtime to an hour ago, truncated to the second,
// and returns it.
func ageTouch(t *testing.T, dir string) time.Time {
	t.Helper()
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(dir+".touch", past, past); err != nil {
		t.Fatal(err)
	}
	return past
}

func TestWithUploads(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	if ws := (&WebSeeder{}).WithUploads(""); ws.up != nil {
		t.Fatal("uploads on without an API secret")
	}
	if e := hook.LastEntry(); e == nil || e.Level != log.InfoLevel || e.Message != "uploads: off, API secret is not set" {
		t.Fatalf("start line %v", e)
	}
	if ws := (&WebSeeder{}).WithUploads(upSecret); ws.up == nil || string(ws.up.secret) != upSecret {
		t.Fatal("uploads off with an API secret")
	}
	if e := hook.LastEntry(); e.Message != "uploads: on" {
		t.Fatalf("start line %q", e.Message)
	}
}

func TestUploadReadServesFile(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a.mkv": "0123456789"})
	past := ageTouch(t, dir)
	rec := serve(ws, httptest.NewRequest(http.MethodHead, "/"+upID+"/Name/a.mkv", nil))
	etag := fmt.Sprintf("\"%x\"", sha1.Sum([]byte(upID+"Name/a.mkv")))
	if rec.Code != 200 || rec.Header().Get("Content-Length") != "10" || rec.Header().Get("Etag") != etag {
		t.Fatalf("HEAD: %d %v", rec.Code, rec.Header())
	}
	if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().After(past) {
		t.Fatal("HEAD of a file left <id>.touch as it was") // HEAD writes no body: only the explicit Touch counts
	}
	req := httptest.NewRequest(http.MethodGet, "/"+upID+"/Name/a.mkv?download=true", nil)
	req.Header.Set("Origin", "https://example.invalid")
	rec = serve(ws, req)
	hdr := rec.Header()
	if rec.Code != 200 || rec.Body.String() != "0123456789" ||
		hdr.Get("Content-Type") != "application/octet-stream" ||
		hdr.Get("Content-Disposition") != `attachment; filename="a.mkv"` ||
		hdr.Get("Access-Control-Allow-Origin") != "*" ||
		hdr.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("GET: %d %q %v", rec.Code, rec.Body.String(), hdr)
	}
	if _, err := os.Stat(filepath.Join(dir, evictGateName)); !os.IsNotExist(err) {
		t.Fatalf("%s in the upload's dir: %v", evictGateName, err)
	}
}

// The torrent queries are answered inside the branch, before any torrent
// code (ws.tm is nil), and none of them touches the dir.
func TestUploadReadQueries(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a.mkv": "0123456789"})
	past := ageTouch(t, dir)
	for _, c := range []struct {
		method, target string
		code           int
		contentType    string
	}{
		{http.MethodGet, "/" + upID + "/?done", 200, ""},
		{http.MethodGet, "/" + upID + "/Name/a.mkv?stats", 404, ""},
		{http.MethodGet, "/" + upID + "/?warmup", 200, "text/event-stream"},
		{http.MethodGet, "/" + upID + "/source.torrent", 404, ""},
		{http.MethodPost, "/" + upID + "/Name/a.mkv", 405, ""},
	} {
		rec := serve(ws, httptest.NewRequest(c.method, c.target, nil))
		if rec.Code != c.code || c.code == 200 && rec.Body.Len() != 0 || c.contentType != "" && rec.Header().Get("Content-Type") != c.contentType {
			t.Errorf("%s %s: %d %q %v, want %d", c.method, c.target, rec.Code, rec.Body.String(), rec.Header(), c.code)
		}
	}
	if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().Equal(past) {
		t.Errorf("<id>.touch touched: %v", fi.ModTime())
	}
}

// A read holds the upload's dir shared: the cleaner cannot drop it in the
// middle of a response, and can once the response is over.
func TestUploadReadHoldsDirFromCleaner(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a.mkv": "0123456789"})
	w := &stuckWriter{h: http.Header{}, writing: make(chan struct{}), unblock: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() {
		ws.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+upID+"/Name/a.mkv", nil))
		close(served)
	}()
	select {
	case <-w.writing:
	case <-served:
		t.Fatal("the file was not served")
	}
	if cleanerDrop(t, dir) {
		t.Error("the cleaner dropped the dir in the middle of a response")
	}
	if _, err := os.Stat(filepath.Join(dir, "files", "Name", "a.mkv")); err != nil {
		t.Error(err)
	}
	close(w.unblock)
	<-served
	if !cleanerDrop(t, dir) {
		t.Fatal("the dir is still held after the response")
	}
}

func TestUploadReadGone(t *testing.T) {
	file := map[string]string{"Name/a.mkv": "0123456789"}
	t.Run("lock removed before", func(t *testing.T) {
		ws, data := upSeeder(t)
		dir := upDir(t, data, upID, file)
		past := ageTouch(t, dir)
		if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
			t.Fatal(err)
		}
		if rec := serve(ws, httptest.NewRequest(http.MethodGet, "/"+upID+"/Name/a.mkv", nil)); rec.Code != 404 {
			t.Errorf("GET: %d", rec.Code)
		}
		if _, err := os.Stat(filepath.Join(dir, ".lock")); !os.IsNotExist(err) {
			t.Errorf(".lock is back: %v", err)
		}
		if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().Equal(past) {
			t.Errorf("<id>.touch touched: %v", fi.ModTime())
		}
	})
	t.Run("lock removed while waiting", func(t *testing.T) {
		ws, data := upSeeder(t)
		dir := upDir(t, data, upID, file)
		lock, err := os.Open(filepath.Join(dir, ".lock"))
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		res := make(chan *httptest.ResponseRecorder)
		go func() { res <- serve(ws, httptest.NewRequest(http.MethodGet, "/"+upID+"/Name/a.mkv", nil)) }()
		time.Sleep(200 * time.Millisecond) // nothing to wait on: the GET opens .lock and waits for it
		if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
			t.Fatal(err)
		}
		_ = lock.Close()
		if rec := <-res; rec.Code != 404 {
			t.Errorf("GET: %d %q, want 404 with files/Name/a.mkv in place", rec.Code, rec.Body.String())
		}
	})
	t.Run("evicted", func(t *testing.T) {
		ws, data := upSeeder(t)
		dir, err := GetDir(data, upID)
		if err != nil {
			t.Fatal(err)
		}
		upVault(t, ws, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/webseed/" + upID + "/Name/a.mkv", "/webseed/" + upID + "/Name/a #1 100%?.mkv":
				if r.URL.RawQuery != "" {
					break
				}
				if r.Method == http.MethodHead {
					return
				}
				http.Redirect(w, r, "http://s3.invalid/copy", http.StatusFound)
				return
			}
			http.NotFound(w, r)
		})
		for _, target := range []string{"/" + upID + "/Name/a.mkv", "/" + upID + "/Name/a%20%231%20100%25%3F.mkv"} {
			rec := serve(ws, httptest.NewRequest(http.MethodGet, target, nil))
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "http://s3.invalid/copy" {
				t.Errorf("GET %s: %d %v", target, rec.Code, rec.Header())
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/"+upID+"/Name/a.mkv", nil)
		req.Header.Set("X-Role", "vault")
		if rec := serve(ws, req); rec.Code != 404 {
			t.Errorf("GET as vault: %d", rec.Code)
		}
		ws.v = nil
		if rec := serve(ws, httptest.NewRequest(http.MethodGet, "/"+upID+"/Name/a.mkv", nil)); rec.Code != 404 {
			t.Errorf("GET without Vault: %d", rec.Code)
		}
		if _, err := os.Stat(dir + ".touch"); !os.IsNotExist(err) {
			t.Errorf("<id>.touch of an evicted upload: %v", err)
		}
	})
}

// A path that is no file of files/ is a miss: 404 without Vault, no byte of a
// part or of another upload, no touch and no error logged.
func TestUploadReadMiss(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a.mkv": "0123456789"})
	if err := os.WriteFile(filepath.Join(dir, "parts", fmt.Sprintf("%x.10", sha1.Sum([]byte("Name/b")))), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	const other = "9c5db5752b7c0fedcba9876543210fedcba98765"
	upDir(t, data, other, map[string]string{"Name/a.mkv": "other-bytes"})
	past := ageTouch(t, dir)
	for _, p := range []string{
		"Name/b",                           // still a part
		"Name",                             // a dir in files/
		"Name/" + strings.Repeat("a", 256), // ENAMETOOLONG
		"Name/a%00b",                       // EINVAL
		"Name/../../../" + other + "/files/Name/a.mkv", // out of files/, not cleaned on the way
	} {
		rec := serve(ws, httptest.NewRequest(http.MethodGet, "/"+upID+"/"+p, nil))
		if rec.Code != 404 || strings.Contains(rec.Body.String(), "other-bytes") {
			t.Errorf("GET %s: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().Equal(past) {
		t.Errorf("<id>.touch touched: %v", fi.ModTime())
	}
	for _, e := range hook.AllEntries() {
		if e.Level == log.ErrorLevel && e.Message == "upload read failed" {
			t.Errorf("logged: %v %v", e.Message, e.Data)
		}
	}
}
