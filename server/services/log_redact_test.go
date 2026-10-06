package services

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
