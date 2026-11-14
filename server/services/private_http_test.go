package services

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urfave/cli"
)

// testTouchMap is a TouchMap over dir.
func testTouchMap(t *testing.T, dir string) *TouchMap {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dir, "")
	return NewTouchMap(cli.NewContext(nil, fs, nil))
}

func TestWriteRefusal(t *testing.T) {
	for _, c := range []struct {
		err        error
		status     int
		reason     string
		retryAfter string
	}{
		{&seederRefusal{403, reasonNotEntitled}, 403, "not_entitled", ""},
		{&seederRefusal{403, reasonBadRecord}, 403, "bad_record", ""},
		{fmt.Errorf("wrapped: %w", &seederRefusal{503, reasonBusy}), 503, "busy", "5"},
		{&seederRefusal{503, reasonStoreUnavailable}, 503, "store_unavailable", "5"},
	} {
		rec := httptest.NewRecorder()
		if !writeRefusal(rec, c.err) || rec.Code != c.status || rec.Header().Get("X-Seeder-Reason") != c.reason || rec.Header().Get("Retry-After") != c.retryAfter {
			t.Errorf("%v: %d %q Retry-After %q", c.err, rec.Code, rec.Header().Get("X-Seeder-Reason"), rec.Header().Get("Retry-After"))
		}
	}
	rec := httptest.NewRecorder()
	if writeRefusal(rec, errors.New("other")) || rec.Code != 200 || len(rec.Header()) != 0 {
		t.Fatal("a non-refusal error was answered")
	}
}

// The refusal reaches the client of serveFile and of ?warmup, not a 500.
func TestServeHTTP_RefusalReachesClient(t *testing.T) {
	env := newPrivateEnv(t, true)
	ws := &WebSeeder{tm: env.tm, tom: testTouchMap(t, env.dir), fcm: zFCM(env.dir), wu: NewWarmup(env.tm),
		tfcm: NewTorrentFileCountMap(&FileStoreMap{}, env.tm.tsm), maxReadahead: 1 << 20}
	srv := httptest.NewServer(newServer(ws).Handler) // the service's middleware: Warmup needs its writer
	t.Cleanup(srv.Close)
	for _, q := range []string{"", "?warmup"} {
		resp, err := http.Get(srv.URL + "/" + env.h + "/payload.bin" + q) // no X-Uid
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 || resp.Header.Get("X-Seeder-Reason") != "account_required" {
			t.Errorf("%q: %d %q, want 403 account_required", q, resp.StatusCode, resp.Header.Get("X-Seeder-Reason"))
		}
	}
}
