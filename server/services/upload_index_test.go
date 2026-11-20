package services

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// getIndex sends GET /<upID>/.
func getIndex(ws *WebSeeder) *httptest.ResponseRecorder {
	return serve(ws, httptest.NewRequest(http.MethodGet, "/"+upID+"/", nil))
}

// wantIndex checks an index answer: 200, application/json and exactly body
// with the encoder's trailing newline.
func wantIndex(t *testing.T, name string, rec *httptest.ResponseRecorder, body string) {
	t.Helper()
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != body+"\n" {
		t.Errorf("%s: %d %v %q, want 200 %q", name, rec.Code, rec.Header(), rec.Body, body)
	}
}

func TestUploadIndex(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/sub/b.srt": "de", "Name/a.mkv": "abc"})
	mustWrite(t, filepath.Join(dir, "parts", fmt.Sprintf("%x.10", sha1.Sum([]byte("Name/c")))), "")
	past := ageTouch(t, dir)
	rec := serve(ws, httptest.NewRequest(http.MethodGet, "/"+upID+"/", nil))
	want := `{"kind":"upload","name":"Name","uids":["` + uidA + `"],"reserved":15,` +
		`"files":[{"path":"Name/a.mkv","size":3},{"path":"Name/sub/b.srt","size":2}]}` + "\n"
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != want {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().Equal(past) {
		t.Fatal("the index touched the upload: refused views would keep it alive")
	}
}

// The first file still in parts/: no root yet, the part's S reserved.
func TestUploadIndexFirstFileInParts(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, nil)
	mustWrite(t, filepath.Join(dir, "parts", partName("Name/a", 10)), "")
	wantIndex(t, "index", getIndex(ws), `{"kind":"upload","name":"","uids":["`+uidA+`"],"reserved":10,"files":[]}`)
}

// owner missing or broken: no uids, the manifest still served.
func TestUploadIndexOwner(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": "abc"})
	owner := filepath.Join(dir, "owner")
	if err := os.Remove(owner); err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"upload","name":"Name","uids":[],"reserved":3,"files":[{"path":"Name/a","size":3}]}`
	wantIndex(t, "no owner", getIndex(ws), want)
	mustWrite(t, owner, "xyz")
	wantIndex(t, "owner xyz", getIndex(ws), want)
}

// reserved is Σ files/ + Σ S of the parts/ names, not the parts' bytes.
func TestUploadIndexReserved(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": strings.Repeat("x", 1<<20)})
	part := filepath.Join(dir, "parts", partName("Name/b", 9663676416))
	mustWrite(t, part, "")
	if err := os.Truncate(part, 9663676416); err != nil { // sparse: 0 bytes written
		t.Fatal(err)
	}
	rec := getIndex(ws)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reserved":9664724992,`) {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
}

// The index walks files/ twice, in reserved and for the list; an error in
// either is 500, never a part of the list.
func TestUploadIndexWalkError(t *testing.T) {
	for _, fail := range []int{1, 2} {
		t.Run(fmt.Sprint("call ", fail), func(t *testing.T) {
			ws, data := upSeeder(t)
			upDir(t, data, upID, map[string]string{"Name/a": "abc", "Name/b": "de"})
			calls := 0
			setHook(t, &walkFiles, func(root string, fn fs.WalkDirFunc) error {
				calls++
				if calls != fail {
					return filepath.WalkDir(root, fn)
				}
				return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
					if err := fn(path, d, err); err != nil {
						return err
					}
					if d.Type().IsRegular() { // after the first file
						return errors.New("walk failed")
					}
					return nil
				})
			})
			rec := getIndex(ws)
			if rec.Code != 500 || strings.HasPrefix(rec.Body.String(), "{") || calls != fail {
				t.Errorf("%d %q after %d walks, want 500 and no list", rec.Code, rec.Body, calls)
			}
		})
	}
}

func TestUploadIndexManyRoots(t *testing.T) {
	ws, data := upSeeder(t)
	upDir(t, data, upID, map[string]string{"A/a": "abc", "B/b": "de"})
	if rec := getIndex(ws); rec.Code != 500 || rec.Body.Len() != 0 {
		t.Errorf("%d %q, want 500 without a body", rec.Code, rec.Body)
	}
}

// reserved and the list are read under the lock of parts/: a part's move to
// files/ between the read of parts/ and the walk of files/ would count it
// twice.
func TestUploadIndexUnderPartsLock(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, nil)
	mustWrite(t, filepath.Join(dir, "parts", partName("Name/a", 4<<20)), strings.Repeat("x", 3<<20))
	tok := writeTok(t, "add", 10<<20)
	entered, hold := make(chan struct{}), make(chan struct{})
	setHook(t, &walkHook, holdOnce("parts", entered, hold))
	idx := make(chan *httptest.ResponseRecorder, 1)
	go func() { idx <- getIndex(ws) }()
	select {
	case <-entered:
	case rec := <-idx:
		t.Fatalf("the index answered %d %q without reading parts/", rec.Code, rec.Body)
	}
	put := make(chan *httptest.ResponseRecorder, 1)
	go func() { put <- putPart(ws, upID, "Name/a", tok, 3<<20, 4<<20, bytes.Repeat([]byte("x"), 1<<20)) }()
	var done *httptest.ResponseRecorder
	select {
	case done = <-put:
		t.Error("the part was done while the index read parts/")
	case <-time.After(300 * time.Millisecond): // nothing to wait on: the PUT waits for parts/
	}
	close(hold)
	wantIndex(t, "index", <-idx, `{"kind":"upload","name":"","uids":["`+uidA+`"],"reserved":4194304,"files":[]}`)
	if done == nil {
		done = <-put
	}
	wantReply(t, "PUT", done, 201, "")
}

// No dir: the manifest comes from Vault, without reserved.
func TestUploadIndexFromVault(t *testing.T) {
	const listing = `{"uids":["` + uidA + `"],"files":[{"path":"/Name/a.mkv","size":3},{"path":"/Name/b.srt","size":2}]}`
	vault := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/resource/"+upID+"/files" {
				http.Error(w, "unexpected request", http.StatusTeapot)
				return
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}
	}
	for _, c := range []struct {
		name, vbody string
		vcode, code int
		body        string
	}{
		{"listed", listing, 200, 200, `{"kind":"upload","name":"Name","uids":["` + uidA + `"],"files":[{"path":"Name/a.mkv","size":3},{"path":"Name/b.srt","size":2}]}`},
		{"nulls", `{"uids":null,"files":null}`, 200, 200, `{"kind":"upload","name":"","uids":[],"files":[]}`},
		{"not in Vault", "", 404, 404, ""},
		{"Vault error", "", 500, 500, ""},
	} {
		ws, _ := upSeeder(t)
		upVault(t, ws, vault(c.vcode, c.vbody))
		rec := getIndex(ws)
		if c.code == 200 {
			wantIndex(t, c.name, rec, c.body)
		} else if rec.Code != c.code || strings.HasPrefix(rec.Body.String(), "{") {
			t.Errorf("%s: %d %q, want %d", c.name, rec.Code, rec.Body, c.code)
		}
	}
	ws, _ := upSeeder(t)
	if rec := getIndex(ws); rec.Code != 404 {
		t.Errorf("Vault not set: %d", rec.Code)
	}
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": "abc"})
	if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	n := upVault(t, ws, vault(404, ""))
	if rec := getIndex(ws); rec.Code != 404 || n.Load() != 1 {
		t.Errorf(".lock gone: %d, Vault asked %d times", rec.Code, n.Load())
	}
}

func TestVaultListFiles(t *testing.T) {
	paths := make(chan string, 3)
	var code atomic.Int32
	code.Store(200)
	ws := &WebSeeder{}
	upVault(t, ws, func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(int(code.Load()))
		_, _ = w.Write([]byte(`{"uids":["` + uidA + `"],"files":[{"path":"/Name/a","size":3}]}`))
	})
	l, err := ws.v.ListFiles(t.Context(), upID)
	want := &VaultListing{UIDs: []string{uidA}, Files: []VaultFile{{Path: "/Name/a", Size: 3}}}
	if path := <-paths; err != nil || !reflect.DeepEqual(l, want) || path != "/resource/"+upID+"/files" {
		t.Errorf("200: %+v, %v at %s", l, err, path)
	}
	code.Store(404)
	if l, err := ws.v.ListFiles(t.Context(), upID); l != nil || err != nil {
		t.Errorf("404: %+v, %v", l, err)
	}
	code.Store(500)
	if l, err := ws.v.ListFiles(t.Context(), upID); l != nil || err == nil {
		t.Errorf("500: %+v, %v", l, err)
	}
}
