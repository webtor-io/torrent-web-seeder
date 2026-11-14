package services

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/sirupsen/logrus/hooks/test"
)

// The values below are made up; "secret" marks each one so a test can tell
// that none of them reached a log line.

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		// thp passes the client's query on: the key first, the token after it.
		"/0a1b/dir/a.mkv?api-key=key-secret-1&token=tok-secret-2&download=true": "/0a1b/dir/a.mkv?api-key=<redacted>&token=<redacted>&download=true",
		"/0a1b/a.mkv?token=tok-secret-2":                                        "/0a1b/a.mkv?token=<redacted>",
		"https://site.example/watch?x=1&api-key=key-secret-3":                   "https://site.example/watch?x=1&api-key=<redacted>",
		// Only a parameter of that very name is a credential.
		"/0a1b/api-key=x/a.mkv?notoken=y&x-api-key=z": "/0a1b/api-key=x/a.mkv?notoken=y&x-api-key=z",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The access log's line of a request carries its URI, query included, and
// its Referer: 998k seeder lines with an API key on 2026-10-05/06 (Loki), 39k
// of them in the Referer.
func TestAccessLogKeepsNoCredentials(t *testing.T) {
	var buf bytes.Buffer
	accessLog.SetOutput(&buf)
	defer accessLog.SetOutput(os.Stderr)

	r := httptest.NewRequest(http.MethodGet, "/0a1b/dir/a.mkv?api-key=key-secret-1&token=tok-secret-2&download=true", nil)
	r.Header.Set("Referer", "https://site.example/watch?api-key=key-secret-3&token=tok-secret-4")
	r.Header.Set("X-Api-Key", "key-secret-5")
	r.Header.Set("X-Token", "tok-secret-6")
	r.Header.Set("Authorization", "Bearer tok-secret-7")
	newServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).Handler.ServeHTTP(httptest.NewRecorder(), r)

	line := buf.String()
	if !strings.Contains(line, "completed handling request") {
		t.Fatalf("no access log line: %q", line)
	}
	if strings.Contains(line, "secret") {
		t.Errorf("a credential in the access log: %q", line)
	}
	for _, want := range []string{
		"/0a1b/dir/a.mkv?api-key=<redacted>&token=<redacted>&download=true",
		"https://site.example/watch?api-key=<redacted>&token=<redacted>",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("%q is not in the access log line %q", want, line)
		}
	}
}

// The torrent library's own line about a failed announce reaches logrus by the
// service's route (anacrolix_log.go) and carries no part of a private
// tracker's key. Up to the fork's webtor-io/torrent#8 that line's urlKey field
// was the whole announce URL.
func TestForkAnnounceFailureLogKeepsNoTrackerKey(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, trackerRefuses)
	}))
	t.Cleanup(srv.Close)
	tc := &TorrentClient{clientSettings: clientSettings{
		swarm:              newSwarmStats(),
		testConfig:         trackerTestConfig,
		maxUnverifiedBytes: -1,
		dataDir:            t.TempDir(),
		allowLocalNetworks: true,
	}}
	t.Cleanup(tc.Close)
	cl, err := tc.Get()
	if err != nil {
		t.Fatal(err)
	}
	_, mi := addTestPayload(t)
	mi.AnnounceList = metainfo.AnnounceList{{srv.URL + "/" + fakePasskey + "/announce?passkey=" + fakePasskey}}
	if _, err := addTorrent(cl, mi); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the library's announce failure in logrus", func() bool {
		for _, e := range hook.AllEntries() {
			if e.Message == "announce failed" && e.Data["source"] == "anacrolix" {
				return true
			}
		}
		return false
	})
	for _, e := range hook.AllEntries() {
		line, err := e.String()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, fakePasskey) {
			t.Errorf("the tracker key in a log line: %q", line)
		}
	}
}
