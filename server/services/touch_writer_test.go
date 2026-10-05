package services

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// agingWriter is a response that runs for hours: by the time its headers are
// out, the .touch the request's start wrote is nine hours old and the
// TouchMap has forgotten writing it, as its 30 s expiry has long before.
type agingWriter struct {
	*httptest.ResponseRecorder
	age func()
}

func (w *agingWriter) WriteHeader(code int) {
	w.age()
	w.ResponseRecorder.WriteHeader(code)
}

// TestLongResponseKeepsTouchFresh: <hash>.touch is the cleaner's clock of a
// torrent dir's last use, and it drops the oldest first. serveFile set it only
// when a request started, so a response that ran for hours left it as old as
// its start. 2026-10-05: free-tier downloads under thp's 5 Mbit/s held nine
// dirs on three nodes for 6-23 h each (ee57f2ad on worker64: one 37.7 GB
// response, 23.3 h), the cleaner skipped them as held 18 times, and once
// released they were the first it would drop.
func TestLongResponseKeepsTouchFresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int64
	}{
		{"cache path", 0},
		// Eviction on: the cache path leaves b to the torrent.
		{"torrent path", 3*zpl - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := zSeed(t)
			ws, _ := zPod(t, dataDir, zMI, tc.budget, 10*time.Minute)
			touch := filepath.Join(dataDir, zHash+".touch")
			hoursAgo := time.Now().Add(-9 * time.Hour)
			w := &agingWriter{ResponseRecorder: httptest.NewRecorder(), age: func() {
				if err := os.Chtimes(touch, hoursAgo, hoursAgo); err != nil {
					t.Error(err)
				}
				ws.tom.Drop(zHash)
			}}
			ws.serveFile(w, httptest.NewRequest(http.MethodGet, "/"+zHash+"/pack/b.mkv", nil), zHash, "pack/b.mkv")
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), zWantB()) {
				t.Fatalf("status %d, %d bytes, b's bytes: %v", w.Code, w.Body.Len(), bytes.Equal(w.Body.Bytes(), zWantB()))
			}
			fi, err := os.Stat(touch)
			if err != nil {
				t.Fatal(err)
			}
			if age := time.Since(fi.ModTime()); age > time.Minute {
				t.Errorf(".touch is %v old after a response that wrote b", age.Round(time.Minute))
			}
		})
	}
}
