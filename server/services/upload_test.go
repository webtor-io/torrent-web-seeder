package services

import (
	"context"
	"crypto/sha1"
	"errors"
	"flag"
	"fmt"
	"io"
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

	"github.com/golang-jwt/jwt/v4"
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

// mustWrite writes body to path, making its parent dirs.
func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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

// quietLog keeps the log out of the test's output until the test ends and
// returns what is logged meanwhile.
func quietLog(t *testing.T) *test.Hook {
	t.Helper()
	std := log.StandardLogger()
	out := std.Out
	std.SetOutput(io.Discard)
	t.Cleanup(func() { std.SetOutput(out) })
	return test.NewGlobal()
}

// wantFailed checks that hook holds exactly n errors, each msg with upID as
// hash, the error and no other field: no token, key or uid. It then empties
// hook.
func wantFailed(t *testing.T, name string, hook *test.Hook, msg string, n int) {
	t.Helper()
	got := 0
	for _, e := range hook.AllEntries() {
		if e.Level != log.ErrorLevel {
			continue
		}
		got++
		if e.Message != msg || len(e.Data) != 2 || e.Data["hash"] != upID || e.Data[log.ErrorKey] == nil {
			t.Errorf("%s: logged %q %v, want %q with hash and error only", name, e.Message, e.Data, msg)
		}
	}
	if got != n {
		t.Errorf("%s: %d errors logged, want %d", name, got, n)
	}
	hook.Reset()
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

// Vault is asked only for a path a write could have stored and no delete took
// back: a clean relative one that is not in deleted. The Vault here has every
// path, so asking it would be served.
func TestUploadReadNeverReachesVault(t *testing.T) {
	const other = "9c5db5752b7c0fedcba9876543210fedcba98765"
	hasAll := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			http.Redirect(w, r, "http://s3.invalid/copy", http.StatusFound)
		}
	}
	get := func(ws *WebSeeder, p string) *httptest.ResponseRecorder {
		return serve(ws, httptest.NewRequest(http.MethodGet, "/"+upID+"/"+p, nil)) // no ServeMux: p arrives as sent
	}
	for _, state := range []string{"evicted", "live"} {
		t.Run("unclean path, "+state, func(t *testing.T) {
			ws, data := upSeeder(t)
			if state == "live" {
				upDir(t, data, upID, map[string]string{"Name/a.mkv": "0123456789"})
			}
			n := upVault(t, ws, hasAll)
			for _, p := range []string{
				"Name/../x",
				"Name/a/../../" + other + "/x", // .. in the middle
				"Name/%2e%2e/x",                // decoded before the branch
				"../" + other + "/Name/a.mkv",  // clean, yet leaves the ID
				"..",
				".",
				"/abs/x", // absolute
				"Name//x",
				"Name/./x",
				"Name/x/",
				"Name/a%00b", // NUL
			} {
				if rec := get(ws, p); rec.Code != 404 || n.Load() != 0 {
					t.Errorf("GET %s: %d, Vault asked %d times", p, rec.Code, n.Load())
					n.Store(0)
				}
			}
		})
	}
	t.Run("deleted path", func(t *testing.T) {
		ws, data := upSeeder(t)
		dir := upDir(t, data, upID, map[string]string{"Name/a.mkv": "0123456789"})
		mustWrite(t, filepath.Join(dir, "deleted"), "\nName/b.mkv\nName/sub\n")
		n := upVault(t, ws, hasAll)
		for _, p := range []string{"Name/b.mkv", "Name/sub/c.mkv"} {
			if rec := get(ws, p); rec.Code != 404 || n.Load() != 0 {
				t.Errorf("GET %s: %d, Vault asked %d times", p, rec.Code, n.Load())
				n.Store(0)
			}
		}
		// The control: a clean path that is neither here nor in deleted.
		if rec := get(ws, "Name/c.mkv"); rec.Code != http.StatusFound || n.Load() == 0 {
			t.Errorf("GET Name/c.mkv: %d, Vault asked %d times", rec.Code, n.Load())
		}
	})
}

// upToken signs c with HS256 under secret.
func upToken(t *testing.T, secret string, c jwt.MapClaims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// claims are web-ui's write claims for upID: hash, up, lim, uid uidA, exp in 15 min.
func claims(up string, lim int64) jwt.MapClaims {
	return jwt.MapClaims{"hash": upID, "up": up, "lim": lim, "uid": uidA, "exp": time.Now().Add(15 * time.Minute).Unix()}
}

// Any token S2 refuses gets 403 without a body for PUT and DELETE, and nothing
// on disk.
func TestUploadWriteToken(t *testing.T) {
	sign := func(c jwt.MapClaims) string { return upToken(t, upSecret, c) }
	with := func(k string, v any) jwt.MapClaims {
		c := claims("new", 10<<20)
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims("new", 10<<20)).SignedString(jwt.UnsafeAllowNoneSignatureType)
	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, claims("new", 10<<20)).SignedString([]byte(upSecret))
	for _, c := range []struct{ name, id, tok string }{
		{"no token", upID, ""},
		{"other hash", upID, sign(with("hash", upID1))},
		{"alg none", upID, none},
		{"HS512, same secret", upID, hs512},
		{"other secret", upID, upToken(t, "other-secret", claims("new", 10<<20))},
		{"expired", upID, sign(with("exp", time.Now().Add(-time.Second).Unix()))},
		{"no exp", upID, sign(with("exp", nil))},
		{"no up", upID, sign(with("up", nil))},
		{"up rw", upID, sign(with("up", "rw"))},
		{"no lim", upID, sign(with("lim", nil))},
		{"lim 0", upID, sign(with("lim", 0))},
		{"lim 1.5", upID, sign(with("lim", 1.5))},
		{"lim over 2^53", upID, sign(with("lim", 1<<54))}, // past exact float64 integers
		{"new without uid", upID, sign(with("uid", nil))},
		{"new uid of 39 hex", upID, sign(with("uid", uidA[:39]))},
		{"new uid upper case", upID, sign(with("uid", strings.ToUpper(uidA)))},
		{"kind 1", upID1, sign(with("hash", upID1))},
		{"kind f", upIDf, sign(with("hash", upIDf))},
	} {
		for _, m := range []string{http.MethodPut, http.MethodDelete} {
			ws, data := upSeeder(t)
			req := httptest.NewRequest(m, "/"+c.id+"/Name/a.mkv?offset=0&size=3", strings.NewReader("abc"))
			req.Header.Set("X-Token", c.tok)
			if rec := serve(ws, req); rec.Code != 403 || rec.Body.Len() != 0 {
				t.Errorf("%s %s: %d %q, want 403 without a body", m, c.name, rec.Code, rec.Body)
			}
			if ents, _ := os.ReadDir(data); len(ents) != 0 {
				t.Errorf("%s %s: %d entries on disk after a refusal", m, c.name, len(ents))
			}
		}
	}
	ws, _ := upSeeder(t) // the valid token gets past S2
	req := httptest.NewRequest(http.MethodPut, "/"+upID+"/Name/a.mkv?offset=0&size=3", strings.NewReader("abc"))
	req.Header.Set("X-Token", sign(claims("new", 10<<20)))
	if rec := serve(ws, req); rec.Code == 403 {
		t.Fatal("a valid up=new token refused")
	}
}

// What S2 passes on: lim of a whole paid cap exactly, and the delete (and
// seal) token, which has no uid and is internal; only JSON true makes it so.
func TestUploadWriteTokenClaims(t *testing.T) {
	ws, _ := upSeeder(t)
	for _, c := range []struct {
		name   string
		claims jwt.MapClaims
		want   writeClaims
	}{
		{"new, 50 GiB", claims("new", 53687091200), writeClaims{up: "new", uid: uidA, lim: 53687091200}},
		{"delete", jwt.MapClaims{"hash": upID, "internal": true, "up": "add", "lim": 1, "exp": time.Now().Add(time.Hour).Unix()}, writeClaims{up: "add", lim: 1, internal: true}},
		{"internal a string", jwt.MapClaims{"hash": upID, "internal": "true", "up": "add", "lim": 1, "exp": time.Now().Add(time.Hour).Unix()}, writeClaims{up: "add", lim: 1}},
	} {
		req := httptest.NewRequest(http.MethodDelete, "/"+upID+"/", nil)
		req.Header.Set("X-Token", upToken(t, upSecret, c.claims))
		got, err := ws.up.writeToken(req, upID)
		if err != nil || *got != c.want {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
}

// A forged X-Info-Hash with the class's prefix and a token for it is no
// upload: it goes the torrent way and writes nothing.
func TestUploadForgedHashNotBranch(t *testing.T) {
	const forged = "9c5db5752b7c/../x"
	ws, data := upSeeder(t)
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String(DataDirFlag, data, "")
	ws.tm, ws.fcm = storeMap(notStored), NewFileCacheMap(cli.NewContext(nil, flags, nil))
	c := claims("new", 10<<20)
	c["hash"] = forged
	req := httptest.NewRequest(http.MethodPut, "/x?offset=0&size=3", strings.NewReader("abc"))
	req.Header.Set("X-Info-Hash", forged)
	req.Header.Set("X-Token", upToken(t, upSecret, c))
	if rec := serve(ws, req); rec.Code != 404 {
		t.Errorf("PUT: %d %q, want the torrent path's 404", rec.Code, rec.Body)
	}
	_ = filepath.WalkDir(data, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Error(err)
		} else if n := d.Name(); n == "x" || n == "files" || n == "owner" {
			t.Errorf("%s on disk", p)
		}
		return nil
	})
}
