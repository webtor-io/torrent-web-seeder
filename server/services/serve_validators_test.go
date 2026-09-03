package services

import (
	"bytes"
	"crypto/sha1"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlite "github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
	"github.com/urfave/cli"
)

// TestServeWithValidatorsKeepsRangeResumable pins the resume contract a
// download manager depends on: a request that carries Range must come back as
// 206 with the bytes asked for, whatever conditional headers ride along.
//
// Support ticket 2026-09-03 (notice 8a5e291f, 5.6 GB .ts): "downloaded 3 GB,
// then everything broke, the file size reset to zero and it started over".
// serveFile used to run its own If-None-Match / If-Modified-Since checks
// before ServeContent. lastMod is the Unix epoch, so `!lastMod.After(t)` was
// true for every parseable date and ANY If-Modified-Since produced a bodyless
// 304 — the Range header was never even looked at. A client resuming with
// If-Modified-Since + Range got no bytes back and started the file again.
//
// This test covers the helper only. Reinstating the hand-rolled blocks in
// serveFile does NOT turn it red — the shortcut returns before the helper is
// ever called, and a first attempt at a negative control here passed with the
// bug restored. TestServeFileDoesNotShortCircuitConditionalRange below is the
// one that guards the defect; this one pins the contract the helper must keep.
func TestServeWithValidatorsKeepsRangeResumable(t *testing.T) {
	const body = "0123456789abcdefghij"
	const etag = `"cafebabe"`
	lastMod := time.Unix(0, 0)

	serve := func(hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/file.ts", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		serveWithValidators(w, req, "file.ts", lastMod, etag, bytes.NewReader([]byte(body)))
		return w
	}

	for _, tt := range []struct {
		name      string
		headers   map[string]string
		wantCode  int
		wantRange string
		wantBody  string
	}{
		{
			name:      "plain range",
			headers:   map[string]string{"Range": "bytes=10-14"},
			wantCode:  http.StatusPartialContent,
			wantRange: "bytes 10-14/20",
			wantBody:  "abcde",
		},
		{
			// The ticket's shape: a resume that also revalidates by date.
			name:      "range plus If-Modified-Since in the past",
			headers:   map[string]string{"Range": "bytes=10-14", "If-Modified-Since": "Thu, 01 Jan 1970 00:00:00 GMT"},
			wantCode:  http.StatusPartialContent,
			wantRange: "bytes 10-14/20",
			wantBody:  "abcde",
		},
		{
			name:      "range plus If-Modified-Since in the present",
			headers:   map[string]string{"Range": "bytes=10-14", "If-Modified-Since": "Wed, 03 Sep 2026 00:00:00 GMT"},
			wantCode:  http.StatusPartialContent,
			wantRange: "bytes 10-14/20",
			wantBody:  "abcde",
		},
		{
			// If-Range is the header a resume is supposed to use, and a
			// matching validator must hand back exactly the missing bytes.
			name:      "range plus matching If-Range",
			headers:   map[string]string{"Range": "bytes=10-14", "If-Range": etag},
			wantCode:  http.StatusPartialContent,
			wantRange: "bytes 10-14/20",
			wantBody:  "abcde",
		},
		{
			// A stale validator means the file may have changed under the
			// client: the whole representation is the correct answer, and
			// restarting the download is then the client's right call.
			name:     "range plus stale If-Range serves the whole file",
			headers:  map[string]string{"Range": "bytes=10-14", "If-Range": `"stale"`},
			wantCode: http.StatusOK,
			wantBody: body,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(tt.headers)
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if got := w.Header().Get("Content-Range"); got != tt.wantRange {
				t.Errorf("Content-Range = %q, want %q", got, tt.wantRange)
			}
			if got := w.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

// TestServeWithValidatorsPublishesStableValidators guards the headers a
// resume is keyed on. The ETag must reach the client (it is what If-Range
// carries on the next attempt) and Last-Modified must stay present, because
// HEAD and GET have to advertise the same validator set — dropping it once
// made players treat HEAD and GET as different resources (vault stream
// incident, 2026-07-06).
func TestServeWithValidatorsPublishesStableValidators(t *testing.T) {
	const etag = `"cafebabe"`
	req := httptest.NewRequest(http.MethodGet, "/file.ts", nil)
	w := httptest.NewRecorder()
	serveWithValidators(w, req, "file.ts", time.Unix(0, 0), etag, bytes.NewReader([]byte("payload")))

	if got := w.Header().Get("Etag"); got != etag {
		t.Errorf("Etag = %q, want %q", got, etag)
	}
	if got := w.Header().Get("Last-Modified"); got == "" {
		t.Error("Last-Modified is absent; HEAD and GET must advertise the same validators")
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes — without it a client will not even try to resume", got)
	}
}

// TestConditionalGetWithoutRangeStillRevalidates keeps the cache behaviour a
// plain conditional GET expects: a matching ETag is answered 304, so a client
// that already holds the file is not made to download it again.
func TestConditionalGetWithoutRangeStillRevalidates(t *testing.T) {
	const etag = `"cafebabe"`
	req := httptest.NewRequest(http.MethodGet, "/file.ts", nil)
	req.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	serveWithValidators(w, req, "file.ts", time.Unix(0, 0), etag, bytes.NewReader([]byte("payload")))

	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d for a matching If-None-Match", w.Code, http.StatusNotModified)
	}
}

// TestServeFileDoesNotShortCircuitConditionalRange is the end-to-end guard for
// the same ticket, at the seam where the defect actually lived: serveFile
// itself. It drives the file-cache branch, which is reachable without a
// torrent client.
//
// Negative control: reinstate either hand-rolled precondition block at the top
// of serveFile and this test fails with 304.
func TestServeFileDoesNotShortCircuitConditionalRange(t *testing.T) {
	const (
		hash = "f07c04a783450dba366e562c253ce56573d7535f"
		path = "episode.ts"
		body = "0123456789abcdefghij"
	)
	dir := t.TempDir()
	torrentDir := filepath.Join(dir, hash)
	if err := os.MkdirAll(torrentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// The cache branch asks sqlite whether the file is complete, then serves
	// it from content/<first two hex of sha1(path)>/<sha1(path)>.
	db, err := sqlite.OpenConn(filepath.Join(torrentDir, ".torrent.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecScript(db, `create table if not exists file_completion("path", unique("path"))`); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Exec(db, `insert or replace into file_completion("path") values(?)`, nil, path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	hexHash := fmt.Sprintf("%x", sha1.Sum([]byte(path)))
	contentDir := filepath.Join(torrentDir, "content", hexHash[:2])
	if err := os.MkdirAll(contentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contentDir, hexHash), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dir, "")
	c := cli.NewContext(nil, fs, nil)
	s := &WebSeeder{fcm: NewFileCacheMap(c), tom: NewTouchMap(c)}

	req := httptest.NewRequest(http.MethodGet, "/"+path, nil)
	req.Header.Set("Range", "bytes=10-14")
	req.Header.Set("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")
	w := httptest.NewRecorder()
	s.serveFile(w, req, hash, path)

	if w.Code == http.StatusNotModified {
		t.Fatal("serveFile answered 304 to a request carrying Range — a resuming client gets no bytes and starts the file over")
	}
	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusPartialContent)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 10-14/20" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 10-14/20")
	}
	if got := w.Body.String(); got != "abcde" {
		t.Errorf("body = %q, want %q", got, "abcde")
	}
}
