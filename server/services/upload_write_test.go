package services

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// putPart sends PUT /<id>/<p>?offset=&size= with X-Token tok and body.
func putPart(ws *WebSeeder, id, p, tok string, offset, size int64, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/%s/%s?offset=%d&size=%d", id, p, offset, size), bytes.NewReader(body))
	req.Header.Set("X-Token", tok)
	return serve(ws, req)
}

// writeTok is a valid write token for upID: up, lim, uid uidA.
func writeTok(t *testing.T, up string, lim int64) string {
	t.Helper()
	return upToken(t, upSecret, claims(up, lim))
}

// partName is the name in parts/ of the part of p, a file of size bytes.
func partName(p string, size int64) string {
	return fmt.Sprintf("%x.%d", sha1.Sum([]byte(p)), size)
}

// setHook sets *v to val until the test ends.
func setHook[T any](t *testing.T, v *T, val T) {
	old := *v
	*v = val
	t.Cleanup(func() { *v = old })
}

// onDisk answers whether path exists, a link not followed.
func onDisk(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// entries counts the entries of dir; a missing dir has none.
func entries(t *testing.T, dir string) int {
	t.Helper()
	e, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(e)
}

// wantReply checks a write's answer against the contract: the code; for 409
// and 413 exactly body as text/plain, for any other code no body; never a
// Location or an X-Seeder-Reason.
func wantReply(t *testing.T, name string, rec *httptest.ResponseRecorder, code int, body string) {
	t.Helper()
	h := rec.Header()
	ok := rec.Code == code && rec.Body.String() == body && h.Get("Location") == "" && h.Get("X-Seeder-Reason") == ""
	if code == 409 || code == 413 {
		ok = ok && h.Get("Content-Type") == "text/plain; charset=utf-8"
	}
	if !ok {
		t.Errorf("%s: %d %q %v, want %d %q", name, rec.Code, rec.Body.String(), h, code, body)
	}
}

// fileSize is the size of path, -1 if it is not there.
func fileSize(path string) int64 {
	fi, err := os.Lstat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// The write contract's codes, one scenario on one seeder.
func TestUploadWriteCodes(t *testing.T) {
	ws, data := upSeeder(t)
	dir, err := GetDir(data, upID)
	if err != nil {
		t.Fatal(err)
	}
	parts := filepath.Join(dir, "parts")
	newTok, addTok := writeTok(t, "new", 10<<20), writeTok(t, "add", 10<<20)
	put := func(p string, offset, size int64, body string) *httptest.ResponseRecorder {
		return putPart(ws, upID, p, addTok, offset, size, []byte(body))
	}
	noParts := func(step string) {
		t.Helper()
		if n := entries(t, parts); n != 0 {
			t.Errorf("%s: %d entries in parts/", step, n)
		}
	}

	wantReply(t, "1", putPart(ws, upID, "Name/a", newTok, 0, 3, []byte("abc")), 201, "")
	wantReply(t, "1a", put("Name/huge", 0, math.MaxInt64, "x"), 413, "size")
	noParts("1a")
	wantReply(t, "2", put("Name/a", 0, 3, "abc"), 201, "")
	wantReply(t, "3", put("Name/a", 0, 4, "abcd"), 409, "exists")
	wantReply(t, "3a", put("Name/a/x", 0, 1, "x"), 400, "")
	if fi, err := os.Lstat(filepath.Join(dir, "files", "Name", "a")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("3a: files/Name/a: %v %v", fi, err)
	} else if b, _ := os.ReadFile(filepath.Join(dir, "files", "Name", "a")); string(b) != "abc" {
		t.Errorf("3a: files/Name/a holds %q", b)
	}
	noParts("3a")
	wantReply(t, "4", put("Other/b", 0, 3, "abc"), 409, "root")
	wantReply(t, "5", put("Name/b", 0, 6, "abc"), 204, "")
	wantReply(t, "6", put("Name/b", 0, 6, "abc"), 409, "3")

	pf, err := os.Open(filepath.Join(parts, partName("Name/b", 6)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(pf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	wantReply(t, "7", put("Name/b", 3, 6, "def"), 409, "3")
	_ = pf.Close()

	wantReply(t, "8", put("Name/b", 3, 6, "def"), 201, "")
	wantReply(t, "9", put("Name/c", 2, 5, "x"), 409, "0")
	wantReply(t, "10 first", put("Name/d", 0, 6, "abc"), 204, "")
	wantReply(t, "10 other size", put("Name/d", 0, 7, "abc"), 409, "exists")

	mustWrite(t, filepath.Join(dir, "deleted"), "\nName/e\n")
	wantReply(t, "11", put("Name/e", 0, 1, "x"), 409, "deleted")
	wantReply(t, "12", put("Name/e/f", 0, 1, "x"), 409, "deleted")
	wantReply(t, "13", put("Name/big", 0, 10<<20, "x"), 413, "size")

	setHook(t, &maxUploadFiles, 3) // a, b and the part of d
	wantReply(t, "14", put("Name/g", 0, 1, "x"), 413, "files")
	maxUploadFiles = 10000 // else 15 gets 413 files, not 507

	setHook(t, &copyPart, func(io.Writer, io.Reader) (int64, error) { return 0, syscall.ENOSPC })
	wantReply(t, "15", put("Name/h", 0, 1, "x"), 507, "")
	copyPart = io.Copy

	for _, c := range []struct {
		name, target, body string
		length             int64
	}{
		{"one segment", "/" + upID + "/Name?offset=0&size=1", "x", 1},
		{"~ in a segment", "/" + upID + "/Name/x~hls?offset=0&size=1", "x", 1},
		{"offset -1", "/" + upID + "/Name/i?offset=-1&size=2", "x", 1},
		{"body over 16 MiB", fmt.Sprintf("/%s/Name/i?offset=0&size=%d", upID, maxPartBytes+1), "x", maxPartBytes + 1},
		{"body past size", "/" + upID + "/Name/i?offset=0&size=2", "abc", 3},
		{"offset past size, near overflow", "/" + upID + "/Name/i?offset=9223372036854775807&size=2", "x", 1},
		{"chunked", "/" + upID + "/Name/i?offset=0&size=2", "ab", -1},
	} {
		req := httptest.NewRequest(http.MethodPut, c.target, strings.NewReader(c.body))
		req.ContentLength = c.length
		req.Header.Set("X-Token", addTok)
		wantReply(t, "16 "+c.name, serve(ws, req), 400, "")
	}
	if onDisk(filepath.Join(parts, partName("Name/i", 2))) {
		t.Error("16: a part of Name/i")
	}

	wantReply(t, "17", putPart(ws, upID, "Name/k", upToken(t, "other-secret", claims("add", 10<<20)), 0, 1, []byte("x")), 403, "")

	if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	wantReply(t, "18", put("Name/j", 0, 1, "x"), 410, "")
}

// OPTIONS is answered without a token, for any kind of the class.
func TestUploadOptions(t *testing.T) {
	ws, _ := upSeeder(t)
	for _, id := range []string{upID, upID1} {
		rec := serve(ws, httptest.NewRequest(http.MethodOptions, "/"+id+"/Name/a", nil))
		h := rec.Header()
		if rec.Code != 204 || rec.Body.Len() != 0 ||
			h.Get("Access-Control-Allow-Methods") != "PUT, DELETE" ||
			h.Get("Access-Control-Allow-Headers") != "content-type" ||
			h.Get("Access-Control-Max-Age") != "600" {
			t.Errorf("OPTIONS %s: %d %q %v", id, rec.Code, rec.Body.String(), h)
		}
	}
	rec := serve(ws, httptest.NewRequest(http.MethodPost, "/"+upID+"/Name/a", nil))
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD, PUT, DELETE, OPTIONS" {
		t.Errorf("POST: %d %v", rec.Code, rec.Header())
	}
}

func TestUploadPathRules(t *testing.T) {
	seg255 := strings.Repeat("s", 255)
	// depth is a path of n segments: Name, n-2 dirs d, the file f.
	depth := func(n int) string { return "Name/" + strings.Repeat("d/", n-2) + "f" }
	// long is a path of n bytes: Name, then segments of up to 255 bytes.
	long := func(n int) string {
		p := "Name"
		for len(p) < n {
			p += "/" + strings.Repeat("l", min(255, n-len(p)-1))
		}
		return p
	}
	label := func(p string) string {
		if len(p) > 80 {
			return fmt.Sprintf("%d bytes, %d segments", len(p), strings.Count(p, "/")+1)
		}
		return p
	}
	if p := depth(32); len(p) != 66 {
		t.Fatalf("depth(32) is %d bytes", len(p))
	}

	for _, p := range []string{"Name/a.mkv", "Name/dir/a.mkv", "Name/a%2Fb", "Name/" + seg255, depth(32)} {
		ws, data := upSeeder(t)
		wantReply(t, label(p), putPart(ws, upID, p, writeTok(t, "new", 10<<20), 0, 1, []byte("x")), 201, "")
		rel, err := url.PathUnescape(p)
		if err != nil {
			t.Fatal(err)
		}
		dir, _ := GetDir(data, upID)
		if b, err := os.ReadFile(filepath.Join(dir, "files", rel)); err != nil || string(b) != "x" {
			t.Errorf("%s: files/%s %q %v", label(p), label(rel), b, err)
		}
	}
	for _, p := range []string{
		"Name", "Name/", "/Name/a", "Name//a", "Name/./a", "Name/%2e%2e/a", "Name/x~hls",
		"Name/a%00b", "Name/a%0Ab", "Name/a%7Fb", "Name/a%FF",
		"Name/" + seg255 + "s", long(1025), depth(33),
	} {
		ws, data := upSeeder(t)
		wantReply(t, label(p), putPart(ws, upID, p, writeTok(t, "new", 10<<20), 0, 1, []byte("x")), 400, "")
		if n := entries(t, data); n != 0 {
			t.Errorf("%s: %d entries in DATA_DIR", label(p), n)
		}
	}

	if !validPath(depth(32)) || validPath(depth(33)) {
		t.Errorf("depth: 32 segments %v, 33 segments %v", validPath(depth(32)), validPath(depth(33)))
	}
	// Not over HTTP: on darwin PATH_MAX (1024) holds the whole path, and
	// <TempDir>/<id>/files/<1024 bytes> is past it.
	if p := long(1024); len(p) != 1024 || !validPath(p) {
		t.Errorf("%s: valid %v", label(p), validPath(p))
	}
	if p := long(1025); len(p) != 1025 || validPath(p) {
		t.Errorf("%s: valid %v", label(p), validPath(p))
	}
}

// The steps of create, in order: the touch before any dir (the cleaner takes a
// dir without <id>.touch as the oldest), owner before .lock (.lock makes the
// dir live: a failure between the two must not leave a live dir without an
// owner).
func TestUploadCreateOrder(t *testing.T) {
	var steps []string
	setHook(t, &createHook, func(s string) error { steps = append(steps, s); return nil })
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	wantReply(t, "new", putPart(ws, upID, "Name/a", writeTok(t, "new", 10<<20), 0, 3, []byte("abc")), 201, "")
	if !slices.Equal(steps, []string{"touch", "dirs", "owner", "lock"}) {
		t.Errorf("steps %v", steps)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "owner")); string(b) != uidA {
		t.Errorf("owner %q", b)
	}
	if !onDisk(dir+".touch") || !onDisk(filepath.Join(dir, ".lock")) || onDisk(filepath.Join(dir, evictGateName)) {
		t.Errorf(".touch %v, .lock %v, %s %v", onDisk(dir+".touch"), onDisk(filepath.Join(dir, ".lock")),
			evictGateName, onDisk(filepath.Join(dir, evictGateName)))
	}

	ws, data = upSeeder(t)
	dir, _ = GetDir(data, upID)
	createHook = func(s string) error {
		if s == "lock" {
			return errors.New("fails between owner and .lock")
		}
		return nil
	}
	wantReply(t, "new, lock fails", putPart(ws, upID, "Name/a", writeTok(t, "new", 10<<20), 0, 3, []byte("abc")), 500, "")
	if b, _ := os.ReadFile(filepath.Join(dir, "owner")); string(b) != uidA {
		t.Errorf("owner %q", b)
	}
	if onDisk(filepath.Join(dir, ".lock")) {
		t.Error(".lock after a failed create")
	}
	wantReply(t, "add after", putPart(ws, upID, "Name/a", writeTok(t, "add", 10<<20), 0, 3, []byte("abc")), 410, "")
	if onDisk(filepath.Join(dir, ".lock")) {
		t.Error(".lock after up=add")
	}
}

func TestUploadOwnerNotOverwritten(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, nil)
	if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	c := claims("new", 10<<20)
	c["uid"] = uidB
	wantReply(t, "new as uidB", putPart(ws, upID, "Name/a", upToken(t, upSecret, c), 0, 3, []byte("abc")), 403, "")
	if b, _ := os.ReadFile(filepath.Join(dir, "owner")); string(b) != uidA {
		t.Errorf("owner %q", b)
	}
}

// up=add on a dir without .lock, or without a dir: 410, and nothing made.
func TestUploadGone(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, nil)
	if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	past := ageTouch(t, dir)
	wantReply(t, "no .lock", putPart(ws, upID, "Name/a", writeTok(t, "add", 10<<20), 0, 3, []byte("abc")), 410, "")
	if onDisk(filepath.Join(dir, ".lock")) || entries(t, filepath.Join(dir, "parts")) != 0 {
		t.Error(".lock or a part after 410")
	}
	if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().Equal(past) {
		t.Errorf("<id>.touch touched: %v", fi.ModTime())
	}

	ws, data = upSeeder(t)
	dir, _ = GetDir(data, upID)
	wantReply(t, "no dir", putPart(ws, upID, "Name/a", writeTok(t, "add", 10<<20), 0, 3, []byte("abc")), 410, "")
	if onDisk(dir+".touch") || onDisk(dir) {
		t.Errorf("<id>.touch %v, dir %v", onDisk(dir+".touch"), onDisk(dir))
	}
}

func TestUploadNoSpace(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, nil)
	tok := writeTok(t, "add", 10<<20)
	body := bytes.Repeat([]byte("x"), 4096)
	setHook(t, &copyPart, func(dst io.Writer, src io.Reader) (int64, error) {
		n, _ := io.CopyN(dst, src, 1000)
		return n, &os.PathError{Op: "write", Path: "part", Err: syscall.ENOSPC}
	})
	wantReply(t, "ENOSPC", putPart(ws, upID, "Name/a", tok, 0, 4096, body), 507, "")
	copyPart = io.Copy
	wantReply(t, "again", putPart(ws, upID, "Name/a", tok, 0, 4096, body), 409, "1000")
	wantReply(t, "the rest", putPart(ws, upID, "Name/a", tok, 1000, 4096, body[1000:]), 201, "")
	if n := fileSize(filepath.Join(dir, "files", "Name", "a")); n != 4096 {
		t.Errorf("files/Name/a: %d bytes", n)
	}
}

// A new file is checked against the token's lim, whatever the dir already
// holds; a started one is not.
func TestUploadLimDowngrade(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, nil)
	sparse := func(path string, size int64) {
		mustWrite(t, path, "")
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}
	sparse(filepath.Join(dir, "files", "Name", "a"), 25<<20)
	sparse(filepath.Join(dir, "parts", partName("Name/b", 5<<20)), 1<<20)
	if sum, _, err := reserved(dir); sum != 30<<20 || err != nil {
		t.Fatalf("reserved %d, %v", sum, err)
	}
	mib := bytes.Repeat([]byte("x"), 1<<20)
	tok20, tok50 := writeTok(t, "add", 20<<20), writeTok(t, "add", 50<<20)
	wantReply(t, "new under 20 MiB", putPart(ws, upID, "Name/c", tok20, 0, 1<<20, mib), 413, "size")
	wantReply(t, "started under 20 MiB", putPart(ws, upID, "Name/b", tok20, 1<<20, 5<<20, mib), 204, "")
	wantReply(t, "new under 50 MiB", putPart(ws, upID, "Name/c", tok50, 0, 1<<20, mib), 201, "")
}

// holdOnce makes the first call of hook stage close entered and wait for
// hold; the others pass.
func holdOnce(stage string, entered, hold chan struct{}) func(string) {
	var once sync.Once
	return func(s string) {
		if s != stage {
			return
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-hold
		}
	}
}

// answers collects n answers from res; the first may come during wait, and
// then hold is closed.
func answers(res chan *httptest.ResponseRecorder, n int, wait time.Duration, hold chan struct{}) []*httptest.ResponseRecorder {
	var got []*httptest.ResponseRecorder
	select {
	case r := <-res:
		got = append(got, r)
	case <-time.After(wait): // nothing to wait on: the second request waits for parts/
	}
	close(hold)
	for len(got) < n {
		got = append(got, <-res)
	}
	return got
}

// The first parts of two new files are checked against lim one after the
// other: two at once would both see the room for one.
func TestUploadFirstPartsSerialized(t *testing.T) {
	ws, data := upSeeder(t)
	upDir(t, data, upID, nil)
	tok := writeTok(t, "add", 10<<20)
	entered, hold := make(chan struct{}), make(chan struct{})
	setHook(t, &walkHook, holdOnce("done", entered, hold))
	body := bytes.Repeat([]byte("x"), 6<<20)
	res := make(chan *httptest.ResponseRecorder, 2)
	go func() { res <- putPart(ws, upID, "Name/a", tok, 0, 6<<20, body) }()
	<-entered
	go func() { res <- putPart(ws, upID, "Name/b", tok, 0, 6<<20, body) }()
	got := answers(res, 2, 300*time.Millisecond, hold)
	slices.SortFunc(got, func(a, b *httptest.ResponseRecorder) int { return a.Code - b.Code })
	wantReply(t, "one", got[0], 201, "")
	wantReply(t, "other", got[1], 413, "size")
}

// A part moves from parts/ to files/ under the lock of parts/: a check of lim
// between the two reads would count it twice.
func TestUploadRenameUnderLock(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, nil)
	mustWrite(t, filepath.Join(dir, "parts", partName("Name/a", 4<<20)), strings.Repeat("x", 3<<20))
	tok := writeTok(t, "add", 10<<20)
	entered, hold := make(chan struct{}), make(chan struct{})
	setHook(t, &walkHook, holdOnce("parts", entered, hold))
	res := make(chan *httptest.ResponseRecorder, 2)
	go func() { res <- putPart(ws, upID, "Name/b", tok, 0, 4<<20, bytes.Repeat([]byte("y"), 4<<20)) }()
	<-entered
	go func() { res <- putPart(ws, upID, "Name/a", tok, 3<<20, 4<<20, bytes.Repeat([]byte("x"), 1<<20)) }()
	for i, r := range answers(res, 2, 300*time.Millisecond, hold) {
		wantReply(t, fmt.Sprint("answer ", i), r, 201, "")
	}
}

// Two first files of a drop at once with one up=new: the one that finds the
// owner of the other gets 403 (not 410, which would stop the whole ID), and
// passes with up=add.
func TestUploadParallelFirstFiles(t *testing.T) {
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	entered, hold := make(chan struct{}), make(chan struct{})
	lockStep := holdOnce("lock", entered, hold)
	setHook(t, &createHook, func(s string) error { lockStep(s); return nil })
	newTok := writeTok(t, "new", 10<<20)
	resA := make(chan *httptest.ResponseRecorder, 1)
	go func() { resA <- putPart(ws, upID, "Name/a", newTok, 0, 3, []byte("abc")) }()
	<-entered
	wantReply(t, "B, new", putPart(ws, upID, "Name/b", newTok, 0, 3, []byte("abc")), 403, "")
	if b, _ := os.ReadFile(filepath.Join(dir, "owner")); string(b) != uidA {
		t.Errorf("owner %q", b)
	}
	close(hold)
	wantReply(t, "A", <-resA, 201, "")
	wantReply(t, "B, add", putPart(ws, upID, "Name/b", writeTok(t, "add", 10<<20), 0, 3, []byte("abc")), 201, "")
}

// A file of 0 bytes is whole at once.
func TestUploadZeroByteFile(t *testing.T) {
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	tok := writeTok(t, "new", 10<<20)
	for _, name := range []string{"first", "again"} {
		wantReply(t, name, putPart(ws, upID, "Name/empty", tok, 0, 0, nil), 201, "")
		if n := fileSize(filepath.Join(dir, "files", "Name", "empty")); n != 0 {
			t.Errorf("%s: files/Name/empty %d", name, n)
		}
		if n := entries(t, filepath.Join(dir, "parts")); n != 0 {
			t.Errorf("%s: %d entries in parts/", name, n)
		}
	}
}

// The dirs below the root count in the files limit, both when a part is made
// and when a done part makes them; the root does not.
func TestUploadFilesLimitCountsDirs(t *testing.T) {
	setHook(t, &maxUploadFiles, 4)
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	newTok, addTok := writeTok(t, "new", 10<<20), writeTok(t, "add", 10<<20)
	parts := filepath.Join(dir, "parts")
	wantReply(t, "1", putPart(ws, upID, "Name/x/y/a", newTok, 0, 0, nil), 201, "") // a, x, x/y: 3
	wantReply(t, "2", putPart(ws, upID, "Name/x/z/b", addTok, 0, 0, nil), 413, "files")
	if onDisk(filepath.Join(dir, "files", "Name", "x", "z")) || entries(t, parts) != 0 {
		t.Error("2: refused, but x/z or a part made")
	}
	wantReply(t, "3", putPart(ws, upID, "Name/x/y/b", addTok, 0, 0, nil), 201, "") // 4: the limit itself
	wantReply(t, "4", putPart(ws, upID, "Name/c", addTok, 0, 0, nil), 413, "files")

	maxUploadFiles = 2 // the root is not counted
	ws, _ = upSeeder(t)
	wantReply(t, "5 a", putPart(ws, upID, "Name/a", newTok, 0, 0, nil), 201, "")
	wantReply(t, "5 b", putPart(ws, upID, "Name/b", addTok, 0, 0, nil), 201, "")
	wantReply(t, "5 c", putPart(ws, upID, "Name/c", addTok, 0, 0, nil), 413, "files")

	maxUploadFiles = 3 // the check when a part is done
	ws, data = upSeeder(t)
	dir, _ = GetDir(data, upID)
	parts = filepath.Join(dir, "parts")
	wantReply(t, "6 p/a starts", putPart(ws, upID, "Name/p/a", newTok, 0, 2, []byte("x")), 204, "")
	wantReply(t, "6 q/b starts", putPart(ws, upID, "Name/q/b", addTok, 0, 2, []byte("x")), 204, "")
	wantReply(t, "6 p/a done", putPart(ws, upID, "Name/p/a", addTok, 1, 2, []byte("y")), 201, "")
	fp, err := os.Open(filepath.Join(parts, partName("Name/q/b", 2)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fp.Close() })
	wantReply(t, "6 q/b done", putPart(ws, upID, "Name/q/b", addTok, 1, 2, []byte("y")), 413, "files")
	if onDisk(filepath.Join(dir, "files", "Name", "q")) || entries(t, parts) != 0 {
		t.Error("6: q made or the part left")
	}
	if fi, err := fp.Stat(); err != nil {
		t.Error(err)
	} else if fi.Size() != 0 {
		t.Errorf("6: the refused part still holds %d bytes", fi.Size())
	}
	if sum, count, err := reserved(dir); sum != 2 || count != 2 || err != nil {
		t.Errorf("6: reserved %d, %d, %v; want 2, 2", sum, count, err)
	}
	wantReply(t, "6 q/b again", putPart(ws, upID, "Name/q/b", addTok, 0, 2, []byte("x")), 413, "files")
	if n := entries(t, parts); n != 0 {
		t.Errorf("6 again: %d entries in parts/", n)
	}
}

// delClaims are the service's delete and seal claims for upID: no uid, lim 1.
func delClaims() jwt.MapClaims {
	return jwt.MapClaims{"hash": upID, "internal": true, "up": "add", "lim": 1, "exp": time.Now().Add(time.Hour).Unix()}
}

// send sends method /<upID>/<p> with X-Token tok and no body.
func send(ws *WebSeeder, method, p, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/"+upID+"/"+p, nil)
	req.Header.Set("X-Token", tok)
	return serve(ws, req)
}

// dlines is deletedLines(dir); a read error fails the test.
func dlines(t *testing.T, dir string) []string {
	t.Helper()
	l, err := deletedLines(dir)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// tomb is tombstoned(dir); a read error fails the test.
func tomb(t *testing.T, dir string) bool {
	t.Helper()
	b, err := tombstoned(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// frozen is sealed(dir); a read error fails the test.
func frozen(t *testing.T, dir string) bool {
	t.Helper()
	b, err := sealed(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// countLine counts the lines l in dir/deleted.
func countLine(t *testing.T, dir, l string) int {
	t.Helper()
	n := 0
	for _, x := range dlines(t, dir) {
		if x == l {
			n++
		}
	}
	return n
}

// emptyDir answers whether path is a dir with no entries.
func emptyDir(t *testing.T, path string) bool {
	t.Helper()
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir() && entries(t, path) == 0
}

// content is what path holds, "" if it cannot be read.
func content(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// openT opens path for reading until the test ends.
func openT(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// fdSize is the size of the file f holds open.
func fdSize(t *testing.T, f *os.File) int64 {
	t.Helper()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// holdCopy makes copyPart close writing once the body is written, then wait
// for hold.
func holdCopy(t *testing.T) (writing, hold chan struct{}) {
	writing, hold = make(chan struct{}), make(chan struct{})
	setHook(t, &copyPart, func(dst io.Writer, src io.Reader) (int64, error) {
		n, err := io.Copy(dst, src)
		close(writing)
		<-hold
		return n, err
	})
	return writing, hold
}

// whileParts runs req while the test holds parts/ of dir, as a part's
// creation or move does: no answer may come in 300 ms. check runs then; the
// lock is let go and the answer returned.
func whileParts(t *testing.T, dir string, req func() *httptest.ResponseRecorder, check func()) *httptest.ResponseRecorder {
	t.Helper()
	pd, err := lockParts(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- req() }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
		t.Errorf("answered %d %q while parts/ was held", rec.Code, rec.Body)
	case <-time.After(300 * time.Millisecond): // nothing to wait on: the request waits for parts/
	}
	check()
	_ = pd.Close()
	if rec == nil {
		rec = <-done
	}
	return rec
}

func TestUploadDeleteWhole(t *testing.T) {
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	files, parts := filepath.Join(dir, "files"), filepath.Join(dir, "parts")
	n := upVault(t, ws, http.NotFound)
	newTok, addTok, delTok := writeTok(t, "new", 10<<20), writeTok(t, "add", 10<<20), upToken(t, upSecret, delClaims())
	del := func(p string) *httptest.ResponseRecorder { return send(ws, http.MethodDelete, p, delTok) }

	wantReply(t, "2 a", putPart(ws, upID, "Name/a", newTok, 0, 3, []byte("abc")), 201, "")
	wantReply(t, "2 b", putPart(ws, upID, "Name/b", newTok, 0, 6, []byte("abc")), 204, "")
	wantReply(t, "3", del(""), 204, "")
	if !emptyDir(t, files) || !emptyDir(t, parts) {
		t.Errorf("4: files/ %d, parts/ %d entries", entries(t, files), entries(t, parts))
	}
	if !tomb(t, dir) || !onDisk(dir+".touch") || !onDisk(filepath.Join(dir, ".lock")) || content(filepath.Join(dir, "owner")) != uidA {
		t.Errorf("4: deleted %q, .touch %v, .lock %v, owner %q", dlines(t, dir), onDisk(dir+".touch"),
			onDisk(filepath.Join(dir, ".lock")), content(filepath.Join(dir, "owner")))
	}
	past := ageTouch(t, dir)
	ws.tom = testTouchMap(t, data) // steps 2–3 cached the Touch

	wantReply(t, "5 new", putPart(ws, upID, "Name/c", newTok, 0, 3, []byte("abc")), 410, "")
	if entries(t, files) != 0 || content(filepath.Join(dir, "owner")) != uidA {
		t.Errorf("5 new: files/ %d entries, owner %q", entries(t, files), content(filepath.Join(dir, "owner")))
	}
	wantReply(t, "5 add", putPart(ws, upID, "Name/c", addTok, 0, 3, []byte("abc")), 410, "")
	if rec := send(ws, http.MethodGet, "Name/a", ""); rec.Code != 404 {
		t.Errorf("5 GET: %d", rec.Code)
	}
	if rec := getIndex(ws); rec.Code != 404 {
		t.Errorf("5 index: %d %q", rec.Code, rec.Body)
	}
	wantReply(t, "5 DELETE file", del("Name/a"), 410, "")
	wantReply(t, "5 DELETE again", del(""), 410, "")

	mustWrite(t, filepath.Join(files, "Name", "left"), "x") // a crash between the line and the wipe
	mustWrite(t, filepath.Join(parts, partName("Name/y", 5)), "")
	wantReply(t, "5a", del(""), 410, "")
	if !emptyDir(t, files) || !emptyDir(t, parts) || countLine(t, dir, "/") != 1 {
		t.Errorf("5a: files/ %d, parts/ %d entries, deleted %q", entries(t, files), entries(t, parts), dlines(t, dir))
	}
	if rec := send(ws, http.MethodGet, "Name/left", ""); rec.Code != 404 {
		t.Errorf("5a GET: %d", rec.Code)
	}

	if n.Load() != 0 {
		t.Errorf("6: Vault asked %d times", n.Load())
	}
	if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().Equal(past) {
		t.Errorf("6: <id>.touch touched: %v", fi.ModTime())
	}
	if !cleanerDrop(t, dir) || onDisk(dir) || onDisk(dir+".touch") {
		t.Error("7: the cleaner did not drop the tombstone")
	}

	ws, data = upSeeder(t)
	dir, _ = GetDir(data, upID)
	wantReply(t, "8", send(ws, http.MethodDelete, "", delTok), 410, "")
	if onDisk(dir + ".touch") {
		t.Error("8: an orphan <id>.touch")
	}
}

func TestUploadDeleteFile(t *testing.T) {
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	files := filepath.Join(dir, "files")
	newTok, addTok, delTok := writeTok(t, "new", 10<<20), writeTok(t, "add", 10<<20), upToken(t, upSecret, delClaims())
	del := func(p string) *httptest.ResponseRecorder { return send(ws, http.MethodDelete, p, delTok) }
	has := func(l string) bool { return slices.Contains(dlines(t, dir), l) }
	for _, p := range []string{"Name/a", "Name/dir/z1", "Name/dir/sub/z2", "Name/dir2/y1"} {
		wantReply(t, p, putPart(ws, upID, p, newTok, 0, 1, []byte("x")), 201, "")
	}

	wantReply(t, "Name/a/x", del("Name/a/x"), 204, "")
	if fi, err := os.Lstat(filepath.Join(files, "Name", "a")); len(dlines(t, dir)) != 0 || err != nil || !fi.Mode().IsRegular() {
		t.Errorf("Name/a/x: deleted %q, files/Name/a %v %v", dlines(t, dir), fi, err)
	}

	wantReply(t, "Name/a", del("Name/a"), 204, "")
	if onDisk(filepath.Join(files, "Name", "a")) || !has("Name/a") {
		t.Errorf("Name/a: on disk %v, deleted %q", onDisk(filepath.Join(files, "Name", "a")), dlines(t, dir))
	}
	wantReply(t, "PUT Name/a", putPart(ws, upID, "Name/a", addTok, 0, 1, []byte("x")), 409, "deleted")

	wantReply(t, "Name/dir/", del("Name/dir/"), 204, "")
	if !has("Name/dir/z1") || !has("Name/dir/sub/z2") || has("Name/dir") || onDisk(filepath.Join(files, "Name", "dir")) {
		t.Errorf("Name/dir/: deleted %q, on disk %v", dlines(t, dir), onDisk(filepath.Join(files, "Name", "dir")))
	}
	wantReply(t, "PUT Name/dir/z1", putPart(ws, upID, "Name/dir/z1", addTok, 0, 1, []byte("x")), 409, "deleted")
	wantReply(t, "PUT Name/dir/new", putPart(ws, upID, "Name/dir/new", addTok, 0, 1, []byte("x")), 201, "")
	if !onDisk(filepath.Join(files, "Name", "dir", "new")) {
		t.Error("Name/dir/new not made")
	}

	wantReply(t, "Name/dir2", del("Name/dir2"), 204, "")
	if !has("Name/dir2/y1") || has("Name/dir2") || onDisk(filepath.Join(files, "Name", "dir2")) {
		t.Errorf("Name/dir2: deleted %q, on disk %v", dlines(t, dir), onDisk(filepath.Join(files, "Name", "dir2")))
	}
	wantReply(t, "PUT Name/dir2/new", putPart(ws, upID, "Name/dir2/new", addTok, 0, 1, []byte("x")), 201, "")

	n := len(dlines(t, dir))
	for _, p := range []string{"Name/missing", "Name/nodir", "Name/a", "Name/missing/"} {
		wantReply(t, p, del(p), 204, "")
		if got := len(dlines(t, dir)); got != n {
			t.Errorf("%s: %d lines in deleted, want %d", p, got, n)
		}
		if p == "Name/nodir" {
			wantReply(t, "PUT Name/nodir/x", putPart(ws, upID, "Name/nodir/x", addTok, 0, 1, []byte("x")), 201, "")
		}
	}

	for _, p := range []string{"Name", "Name/"} {
		wantReply(t, p, del(p), 400, "")
	}
	if !onDisk(filepath.Join(files, "Name")) {
		t.Error("files/Name removed")
	}
	wantReply(t, "up=new", send(ws, http.MethodDelete, "Name/dir/new", newTok), 403, "")
	if !onDisk(filepath.Join(files, "Name", "dir", "new")) {
		t.Error("up=new deleted Name/dir/new")
	}

	ws, _ = upSeeder(t)
	wantReply(t, "no dir", send(ws, http.MethodDelete, "Name/a", delTok), 410, "")
	ws, data = upSeeder(t)
	dir = upDir(t, data, upID, map[string]string{"Name/a": "abc"})
	if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	wantReply(t, "no .lock", send(ws, http.MethodDelete, "Name/a", delTok), 410, "")
	if onDisk(filepath.Join(dir, ".lock")) {
		t.Error(".lock made by a DELETE")
	}
}

func TestUploadDeleteFolderWaitsForParts(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/dir/z1": "z"})
	z1 := filepath.Join(dir, "files", "Name", "dir", "z1")
	delTok := upToken(t, upSecret, delClaims())
	rec := whileParts(t, dir, func() *httptest.ResponseRecorder { return send(ws, http.MethodDelete, "Name/dir/", delTok) }, func() {
		if !onDisk(z1) || len(dlines(t, dir)) != 0 {
			t.Errorf("while parts/ is held: z1 %v, deleted %q", onDisk(z1), dlines(t, dir))
		}
	})
	wantReply(t, "DELETE", rec, 204, "")
	if !slices.Contains(dlines(t, dir), "Name/dir/z1") || onDisk(filepath.Dir(z1)) {
		t.Errorf("deleted %q, files/Name/dir %v", dlines(t, dir), onDisk(filepath.Dir(z1)))
	}
}

func TestUploadDeleteFolderWalkError(t *testing.T) {
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	tok := writeTok(t, "new", 10<<20)
	for _, p := range []string{"Name/dir/z1", "Name/dir/sub/z2"} {
		wantReply(t, p, putPart(ws, upID, p, tok, 0, 1, []byte("z")), 201, "")
	}
	n0 := len(dlines(t, dir))
	setHook(t, &walkFiles, func(root string, fn fs.WalkDirFunc) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err := fn(path, d, err); err != nil {
				return err
			}
			if d.Type().IsRegular() { // after the first file
				return errors.New("walk")
			}
			return nil
		})
	})
	wantReply(t, "DELETE", send(ws, http.MethodDelete, "Name/dir/", upToken(t, upSecret, delClaims())), 500, "")
	if len(dlines(t, dir)) != n0 {
		t.Errorf("deleted %q", dlines(t, dir))
	}
	for _, p := range []string{"z1", "sub/z2"} {
		if !onDisk(filepath.Join(dir, "files", "Name", "dir", p)) {
			t.Errorf("%s removed", p)
		}
	}
}

// A record that ENOSPC tore off without its "\n" does not glue onto the next.
func TestUploadDeletedTornTail(t *testing.T) {
	dir := upDir(t, t.TempDir(), upID, nil)
	if err := os.WriteFile(filepath.Join(dir, "deleted"), []byte("Name/x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendDeleted(dir, "Name/dir/z1"); err != nil {
		t.Fatal(err)
	}
	if lines := dlines(t, dir); !inDeleted(lines, "Name/dir/z1") || !inDeleted(lines, "Name/x") {
		t.Errorf("deleted %q", lines)
	}
	if err := appendDeleted(dir, "/"); err != nil {
		t.Fatal(err)
	}
	if !tomb(t, dir) {
		t.Errorf("no tombstone in %q", dlines(t, dir))
	}
}

func TestUploadNewPartOnTombstone(t *testing.T) {
	for _, c := range []struct {
		line string
		want upReply
	}{
		{"/", upReply{code: 410}},
		{"~", upReply{code: 409, body: "frozen"}},
	} {
		dir := upDir(t, t.TempDir(), upID, nil)
		if err := appendDeleted(dir, c.line); err != nil {
			t.Fatal(err)
		}
		part := filepath.Join(dir, "parts", partName("Name/a", 3))
		f, err := newPart(dir, "Name/a", part, 0, 3, 10<<20)
		if f != nil {
			_ = f.Close()
		}
		if r := (*upReply)(nil); !errors.As(err, &r) || *r != c.want {
			t.Errorf("%q: %v, want %v", c.line, err, &c.want)
		}
		if onDisk(part) {
			t.Errorf("%q: the part made", c.line)
		}
	}
}

// The whole upload deleted while a part is written: the part ends 410.
func TestUploadDeleteWholeDuringPart(t *testing.T) {
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	writing, hold := holdCopy(t)
	res := make(chan *httptest.ResponseRecorder, 1)
	go func() { res <- putPart(ws, upID, "Name/a", writeTok(t, "new", 10<<20), 0, 3, []byte("abc")) }()
	<-writing
	wantReply(t, "DELETE", send(ws, http.MethodDelete, "", upToken(t, upSecret, delClaims())), 204, "")
	close(hold)
	wantReply(t, "PUT", <-res, 410, "")
	if !emptyDir(t, filepath.Join(dir, "files")) || !emptyDir(t, filepath.Join(dir, "parts")) {
		t.Errorf("files/ %d, parts/ %d entries", entries(t, filepath.Join(dir, "files")), entries(t, filepath.Join(dir, "parts")))
	}
}

// The seal, one scenario on one seeder.
func TestUploadSeal(t *testing.T) {
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	files, parts := filepath.Join(dir, "files"), filepath.Join(dir, "parts")
	partB := filepath.Join(parts, partName("Name/b", 6))
	n := upVault(t, ws, http.NotFound)
	newTok, addTok, delTok := writeTok(t, "new", 10<<20), writeTok(t, "add", 10<<20), upToken(t, upSecret, delClaims())
	seal := func(tok string) *httptest.ResponseRecorder { return send(ws, http.MethodPut, "", tok) }
	open := func(step string) {
		t.Helper()
		if frozen(t, dir) || !onDisk(partB) {
			t.Errorf("%s: sealed %v, part of Name/b %v", step, frozen(t, dir), onDisk(partB))
		}
	}

	wantReply(t, "2 a", putPart(ws, upID, "Name/a", newTok, 0, 3, []byte("abc")), 201, "")
	wantReply(t, "2 z1", putPart(ws, upID, "Name/dir/z1", newTok, 0, 1, []byte("z")), 201, "")
	wantReply(t, "2 b", putPart(ws, upID, "Name/b", newTok, 0, 6, []byte("abc")), 204, "")
	wantReply(t, "3", seal(newTok), 403, "")
	open("3")
	for _, c := range []struct{ name, query, body string }{
		{"a file's PUT", "?offset=0&size=3", "abc"},
		{"offset", "?offset=0", ""},
		{"size", "?size=0", ""},
		{"body", "", "abc"},
	} {
		req := httptest.NewRequest(http.MethodPut, "/"+upID+"/"+c.query, strings.NewReader(c.body))
		req.Header.Set("X-Token", addTok)
		wantReply(t, "3a "+c.name, serve(ws, req), 400, "")
		open("3a " + c.name)
	}

	wantReply(t, "4", seal(delTok), 204, "")
	if l := dlines(t, dir); !slices.Equal(l, []string{"~"}) || !emptyDir(t, parts) {
		t.Errorf("4: deleted %q, parts/ %d entries", l, entries(t, parts))
	}
	for _, p := range []string{filepath.Join(files, "Name", "a"), filepath.Join(files, "Name", "dir", "z1"), dir + ".touch", filepath.Join(dir, ".lock")} {
		if !onDisk(p) {
			t.Errorf("4: %s gone", p)
		}
	}
	if o := content(filepath.Join(dir, "owner")); o != uidA {
		t.Errorf("4: owner %q", o)
	}
	past := ageTouch(t, dir)
	ws.tom = testTouchMap(t, data) // steps 2–4 cached the Touch

	wantReply(t, "5 new file", putPart(ws, upID, "Name/c", addTok, 0, 3, []byte("abc")), 409, "frozen")
	if onDisk(filepath.Join(files, "Name", "c")) || entries(t, parts) != 0 {
		t.Error("5: Name/c or a part made")
	}
	wantReply(t, "5 up=new", putPart(ws, upID, "Name/c", newTok, 0, 3, []byte("abc")), 409, "frozen")
	wantReply(t, "5 started part", putPart(ws, upID, "Name/b", addTok, 3, 6, []byte("def")), 409, "frozen")
	wantReply(t, "5 done path", putPart(ws, upID, "Name/a", addTok, 0, 3, []byte("abc")), 409, "frozen")
	wantReply(t, "5 DELETE file", send(ws, http.MethodDelete, "Name/a", delTok), 409, "frozen")
	wantReply(t, "5 DELETE folder", send(ws, http.MethodDelete, "Name/dir/", delTok), 409, "frozen")
	if !onDisk(filepath.Join(files, "Name", "a")) || !onDisk(filepath.Join(files, "Name", "dir", "z1")) || !slices.Equal(dlines(t, dir), []string{"~"}) {
		t.Errorf("5: deleted %q after the refused DELETEs", dlines(t, dir))
	}
	if fi, _ := os.Stat(dir + ".touch"); !fi.ModTime().Equal(past) {
		t.Errorf("5: <id>.touch touched: %v", fi.ModTime())
	}

	if rec := send(ws, http.MethodGet, "Name/a", ""); rec.Code != 200 || rec.Body.String() != "abc" {
		t.Errorf("6 GET: %d %q", rec.Code, rec.Body)
	}
	wantIndex(t, "6 index", getIndex(ws), `{"kind":"upload","name":"Name","uids":["`+uidA+`"],"reserved":4,`+
		`"files":[{"path":"Name/a","size":3},{"path":"Name/dir/z1","size":1}]}`)
	if n.Load() != 0 {
		t.Errorf("6: Vault asked %d times", n.Load())
	}

	mustWrite(t, filepath.Join(parts, partName("Name/x", 5)), "") // a crash between the line and the wipe
	wantReply(t, "7", seal(delTok), 204, "")
	if entries(t, parts) != 0 || countLine(t, dir, "~") != 1 {
		t.Errorf("7: parts/ %d entries, deleted %q", entries(t, parts), dlines(t, dir))
	}

	wantReply(t, "8 DELETE", send(ws, http.MethodDelete, "", delTok), 204, "")
	wantReply(t, "8 seal", seal(delTok), 410, "")
	if rec := send(ws, http.MethodGet, "Name/a", ""); rec.Code != 404 {
		t.Errorf("8 GET: %d", rec.Code)
	}
	wantReply(t, "8 PUT", putPart(ws, upID, "Name/c", addTok, 0, 3, []byte("abc")), 410, "")

	ws, data = upSeeder(t)
	dir, _ = GetDir(data, upID)
	wantReply(t, "9 no dir", send(ws, http.MethodPut, "", delTok), 410, "")
	if onDisk(dir + ".touch") {
		t.Error("9: an orphan <id>.touch")
	}
	ws, data = upSeeder(t)
	dir = upDir(t, data, upID, nil)
	if err := os.Remove(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	wantReply(t, "9 no .lock", send(ws, http.MethodPut, "", delTok), 410, "")
	if onDisk(filepath.Join(dir, ".lock")) {
		t.Error("9: .lock made by a seal")
	}
}

// The seal while a part is written: the part ends 409 frozen.
func TestUploadSealDuringPart(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": "abc"})
	files := filepath.Join(dir, "files")
	writing, hold := holdCopy(t)
	res := make(chan *httptest.ResponseRecorder, 1)
	go func() { res <- putPart(ws, upID, "Name/b", writeTok(t, "add", 10<<20), 0, 3, []byte("abc")) }()
	<-writing
	wantReply(t, "seal", send(ws, http.MethodPut, "", upToken(t, upSecret, delClaims())), 204, "")
	close(hold)
	wantReply(t, "PUT", <-res, 409, "frozen")
	if onDisk(filepath.Join(files, "Name", "b")) || entries(t, filepath.Join(dir, "parts")) != 0 || content(filepath.Join(files, "Name", "a")) != "abc" {
		t.Errorf("Name/b %v, parts/ %d entries, Name/a %q", onDisk(filepath.Join(files, "Name", "b")),
			entries(t, filepath.Join(dir, "parts")), content(filepath.Join(files, "Name", "a")))
	}
}

func TestUploadSealWaitsForParts(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": "abc"})
	delTok := upToken(t, upSecret, delClaims())
	rec := whileParts(t, dir, func() *httptest.ResponseRecorder { return send(ws, http.MethodPut, "", delTok) }, func() {
		if frozen(t, dir) {
			t.Error("sealed while parts/ was held")
		}
	})
	wantReply(t, "seal", rec, 204, "")
	if !frozen(t, dir) {
		t.Error("not sealed")
	}
}

// A file's DELETE waits for parts/ and then sees what got there first: a seal
// (409 frozen) or the whole upload's deletion (410).
func TestUploadDeleteFileWaitsForParts(t *testing.T) {
	for _, c := range []struct {
		first string
		code  int
		body  string
	}{
		{"~", 409, "frozen"},
		{"/", 410, ""},
	} {
		ws, data := upSeeder(t)
		dir := upDir(t, data, upID, map[string]string{"Name/a": "abc"})
		a := filepath.Join(dir, "files", "Name", "a")
		delTok := upToken(t, upSecret, delClaims())
		rec := whileParts(t, dir, func() *httptest.ResponseRecorder { return send(ws, http.MethodDelete, "Name/a", delTok) }, func() {
			if !onDisk(a) || len(dlines(t, dir)) != 0 {
				t.Errorf("%q, while parts/ is held: Name/a %v, deleted %q", c.first, onDisk(a), dlines(t, dir))
			}
			if err := appendDeleted(dir, c.first); err != nil {
				t.Fatal(err)
			}
		})
		wantReply(t, c.first, rec, c.code, c.body)
		if !onDisk(a) || slices.Contains(dlines(t, dir), "Name/a") {
			t.Errorf("%q: Name/a %v, deleted %q", c.first, onDisk(a), dlines(t, dir))
		}
	}
}

// A seal record torn by ENOSPC at any byte is no tombstone, before and after
// the next record ends it.
func TestUploadSealTornTail(t *testing.T) {
	dir := upDir(t, t.TempDir(), upID, nil)
	rec := "\n" + sealLine + "\n"
	for k := 1; k < len(rec); k++ {
		if err := os.WriteFile(filepath.Join(dir, "deleted"), []byte("Name/x\n"+rec[:k]), 0o644); err != nil {
			t.Fatal(err)
		}
		if tomb(t, dir) {
			t.Errorf("k=%d: torn seal reads as a tombstone", k)
		}
		if err := appendDeleted(dir, "Name/y"); err != nil {
			t.Fatal(err)
		}
		if tomb(t, dir) || !inDeleted(dlines(t, dir), "Name/x") {
			t.Errorf("k=%d: tombstone %v, deleted %q", k, tomb(t, dir), dlines(t, dir))
		}
	}
	if strings.Contains(sealLine, "/") || validPath("Name/"+sealLine) {
		t.Errorf("seal line %q may be a path", sealLine)
	}
}

// A file open before its deletion is empty after it; what the deletion does
// not take stays whole.
func TestUploadDeleteZeroesOpenFiles(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": "abc", "Name/keep": "keep", "Name/dir/z1": "z", "Name/dir/sub/z2": "zz"})
	files := filepath.Join(dir, "files")
	part := filepath.Join(dir, "parts", partName("Name/b", 6))
	mustWrite(t, part, "abc")
	delTok := upToken(t, upSecret, delClaims())
	del := func(p string) *httptest.ResponseRecorder { return send(ws, http.MethodDelete, p, delTok) }
	fa, fk := openT(t, filepath.Join(files, "Name", "a")), openT(t, filepath.Join(files, "Name", "keep"))
	fz1, fz2 := openT(t, filepath.Join(files, "Name", "dir", "z1")), openT(t, filepath.Join(files, "Name", "dir", "sub", "z2"))
	fp := openT(t, part)

	wantReply(t, "3", del("Name/a"), 204, "")
	if _, err := fa.Read(make([]byte, 1)); fdSize(t, fa) != 0 || err != io.EOF {
		t.Errorf("3: Name/a %d bytes, read %v", fdSize(t, fa), err)
	}
	if fdSize(t, fk) != 4 || fdSize(t, fz1) != 1 || fdSize(t, fp) != 3 {
		t.Errorf("3: keep %d, z1 %d, part %d", fdSize(t, fk), fdSize(t, fz1), fdSize(t, fp))
	}
	wantReply(t, "4", del("Name/dir/"), 204, "")
	if fdSize(t, fz1) != 0 || fdSize(t, fz2) != 0 || fdSize(t, fk) != 4 || fdSize(t, fp) != 3 {
		t.Errorf("4: z1 %d, z2 %d, keep %d, part %d", fdSize(t, fz1), fdSize(t, fz2), fdSize(t, fk), fdSize(t, fp))
	}
	wantReply(t, "5", del(""), 204, "")
	if fdSize(t, fk) != 0 || fdSize(t, fp) != 0 {
		t.Errorf("5: keep %d, part %d", fdSize(t, fk), fdSize(t, fp))
	}
	mustWrite(t, filepath.Join(files, "Name", "left"), "x")
	fl := openT(t, filepath.Join(files, "Name", "left"))
	wantReply(t, "6", del(""), 410, "")
	if fdSize(t, fl) != 0 {
		t.Errorf("6: left %d", fdSize(t, fl))
	}

	ws, data = upSeeder(t)
	dir = upDir(t, data, upID, map[string]string{"Name/a": "abc"})
	part = filepath.Join(dir, "parts", partName("Name/b", 6))
	mustWrite(t, part, "abc")
	fa, fp = openT(t, filepath.Join(dir, "files", "Name", "a")), openT(t, part)
	wantReply(t, "7", send(ws, http.MethodPut, "", delTok), 204, "")
	if fdSize(t, fp) != 0 || fdSize(t, fa) != 3 {
		t.Errorf("7: part %d, Name/a %d", fdSize(t, fp), fdSize(t, fa))
	}
}

// A deletion takes links off and leaves their targets: the last component of
// the path and the links in a deleted tree are not followed.
func TestUploadDeleteSkipsSymlinks(t *testing.T) {
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": "abc", "Name/dir/z1": "z"})
	files := filepath.Join(dir, "files")
	out, outDir := filepath.Join(t.TempDir(), "out"), t.TempDir()
	mustWrite(t, out, "keep")
	mustWrite(t, filepath.Join(outDir, "f"), "keep")
	link := filepath.Join(files, "Name", "link")
	for l, target := range map[string]string{link: out, filepath.Join(files, "Name", "dir", "link"): out, filepath.Join(files, "Name", "dir", "dlink"): outDir} {
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
	}
	kept := func(step string) {
		t.Helper()
		if content(out) != "keep" || content(filepath.Join(outDir, "f")) != "keep" {
			t.Errorf("%s: out %q, outDir/f %q", step, content(out), content(filepath.Join(outDir, "f")))
		}
	}
	del := func(p string) *httptest.ResponseRecorder {
		return send(ws, http.MethodDelete, p, upToken(t, upSecret, delClaims()))
	}

	if err := zeroFile(link); !errors.Is(err, syscall.ELOOP) || !onDisk(link) {
		t.Errorf("2: zeroFile of a link: %v, link %v", err, onDisk(link))
	}
	kept("2")
	wantReply(t, "3", del("Name/link"), 204, "")
	if onDisk(link) || slices.Contains(dlines(t, dir), "Name/link") {
		t.Errorf("3: link %v, deleted %q", onDisk(link), dlines(t, dir))
	}
	kept("3")
	wantReply(t, "4", del("Name/dir/"), 204, "")
	if l := dlines(t, dir); onDisk(filepath.Join(files, "Name", "dir")) || !slices.Contains(l, "Name/dir/z1") ||
		slices.Contains(l, "Name/dir/link") || slices.Contains(l, "Name/dir/dlink/f") {
		t.Errorf("4: files/Name/dir %v, deleted %q", onDisk(filepath.Join(files, "Name", "dir")), l)
	}
	kept("4")
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	wantReply(t, "5", del(""), 204, "")
	if !emptyDir(t, files) {
		t.Errorf("5: files/ %d entries", entries(t, files))
	}
	kept("5")
}

// Only what is unlinked is emptied: a file whose unlink fails keeps its
// content at its path.
func TestUploadDeleteKeepsContentAtPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the dir's permissions: unlink would not fail, the order unlink → ftruncate is unchecked")
	}
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/dir/z1": "abc"})
	d := filepath.Join(dir, "files", "Name", "dir")
	z1 := filepath.Join(d, "z1")
	if err := os.Chmod(d, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	delTok := upToken(t, upSecret, delClaims())
	wantReply(t, "2", send(ws, http.MethodDelete, "Name/dir/z1", delTok), 500, "")
	if content(z1) != "abc" || !slices.Contains(dlines(t, dir), "Name/dir/z1") {
		t.Errorf("3: z1 %q, deleted %q", content(z1), dlines(t, dir))
	}
	if rec := send(ws, http.MethodGet, "Name/dir/z1", ""); rec.Code != 200 || rec.Body.String() != "abc" {
		t.Errorf("3 GET: %d %q", rec.Code, rec.Body)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	wantReply(t, "4", send(ws, http.MethodDelete, "Name/dir/z1", delTok), 204, "")
	if onDisk(z1) || countLine(t, dir, "Name/dir/z1") != 1 {
		t.Errorf("4: z1 %v, deleted %q", onDisk(z1), dlines(t, dir))
	}
}

// A failed zeroing in the middle does not stop the deletion.
func TestUploadDeleteZeroError(t *testing.T) {
	zero := zeroFile
	failOn := func(suffix string) func(string) error {
		return func(path string) error {
			if strings.HasSuffix(path, suffix) {
				return errors.New("zero")
			}
			return zero(path)
		}
	}
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/dir/sub/z2": "zz", "Name/dir/z1": "abc"})
	f1 := openT(t, filepath.Join(dir, "files", "Name", "dir", "z1"))
	delTok := upToken(t, upSecret, delClaims())
	setHook(t, &zeroFile, failOn("sub/z2")) // the walk meets it first: sub < z1
	wantReply(t, "3", send(ws, http.MethodDelete, "Name/dir/", delTok), 500, "")
	if l := dlines(t, dir); !slices.Contains(l, "Name/dir/z1") || !slices.Contains(l, "Name/dir/sub/z2") ||
		onDisk(filepath.Join(dir, "files", "Name", "dir")) || fdSize(t, f1) != 0 {
		t.Errorf("4: deleted %q, files/Name/dir %v, z1 %d", l, onDisk(filepath.Join(dir, "files", "Name", "dir")), fdSize(t, f1))
	}
	wantReply(t, "5", putPart(ws, upID, "Name/dir/z1", writeTok(t, "add", 10<<20), 0, 3, []byte("abc")), 409, "deleted")
	zeroFile = zero
	wantReply(t, "6", send(ws, http.MethodDelete, "Name/dir/", delTok), 204, "")

	ws, data = upSeeder(t)
	dir = upDir(t, data, upID, map[string]string{"Name/a": "abc"})
	mustWrite(t, filepath.Join(dir, "parts", partName("Name/y", 5)), "")
	zeroFile = failOn("Name/a")
	wantReply(t, "7", send(ws, http.MethodDelete, "", delTok), 500, "")
	if !tomb(t, dir) || !emptyDir(t, filepath.Join(dir, "files")) || !emptyDir(t, filepath.Join(dir, "parts")) {
		t.Errorf("7: tombstone %v, files/ %d, parts/ %d entries", tomb(t, dir),
			entries(t, filepath.Join(dir, "files")), entries(t, filepath.Join(dir, "parts")))
	}
	wantReply(t, "7 again", send(ws, http.MethodDelete, "", delTok), 410, "")
}

// The limit of deleted: a path record that would take it past is refused
// whole and before it is written; the tombstone and the seal pass it.
func TestUploadDeletedLimit(t *testing.T) {
	setHook(t, &maxDeletedBytes, 16)
	ws, data := upSeeder(t)
	dir := upDir(t, data, upID, map[string]string{"Name/a": "abc", "Name/b": "b", "Name/cc": "c", "Name/dir/z1": "z", "Name/dir/z2": "zz"})
	files := filepath.Join(dir, "files")
	delTok := upToken(t, upSecret, delClaims())
	del := func(p string) *httptest.ResponseRecorder { return send(ws, http.MethodDelete, p, delTok) }
	dsize := func(step string, want int64) {
		t.Helper()
		if n := max(fileSize(filepath.Join(dir, "deleted")), 0); n != want {
			t.Errorf("%s: deleted %d bytes, want %d", step, n, want)
		}
	}

	wantReply(t, "2", del("Name/a"), 204, "")
	dsize("2", 8)
	wantReply(t, "2a", del("Name/cc"), 413, "deletions") // 8 + 9 > 16: the leading "\n" counts
	dsize("2a", 8)
	if !onDisk(filepath.Join(files, "Name", "cc")) {
		t.Error("2a: Name/cc deleted")
	}
	wantReply(t, "3", del("Name/dir/"), 413, "deletions")
	dsize("3", 8)
	if !onDisk(filepath.Join(files, "Name", "dir", "z1")) || !onDisk(filepath.Join(files, "Name", "dir", "z2")) {
		t.Error("3: a file of Name/dir deleted")
	}
	if rec := send(ws, http.MethodGet, "Name/dir/z1", ""); rec.Code != 200 {
		t.Errorf("3 GET: %d", rec.Code)
	}
	wantReply(t, "4", del("Name/dir/z1"), 413, "deletions")
	dsize("4", 8)
	if !onDisk(filepath.Join(files, "Name", "dir", "z1")) {
		t.Error("4: Name/dir/z1 deleted")
	}
	wantReply(t, "5", del("Name/b"), 204, "") // 8 + 8 = 16: the limit itself
	dsize("5", 16)
	mustWrite(t, filepath.Join(files, "Name", "b"), "b") // an unlink that failed after the line
	wantReply(t, "5a", del("Name/b"), 204, "")
	dsize("5a", 16)
	if onDisk(filepath.Join(files, "Name", "b")) {
		t.Error("5a: Name/b left")
	}
	wantReply(t, "6", del("Name/missing"), 204, "")
	dsize("6", 16)
	wantReply(t, "7", send(ws, http.MethodPut, "", delTok), 204, "")
	dsize("7", 19)
	if !frozen(t, dir) {
		t.Error("7: not sealed")
	}
	wantReply(t, "8", del(""), 204, "")
	dsize("8", 22)
	if !tomb(t, dir) || !emptyDir(t, files) {
		t.Errorf("8: tombstone %v, files/ %d entries", tomb(t, dir), entries(t, files))
	}
}

// A file's DELETE leaves its dirs counted: emptying files one by one cannot
// grow dirs past the limit; a folder's DELETE takes them out.
func TestUploadDeleteFileKeepsDirsCounted(t *testing.T) {
	setHook(t, &maxUploadFiles, 3)
	ws, data := upSeeder(t)
	dir, _ := GetDir(data, upID)
	files := filepath.Join(dir, "files")
	addTok, delTok := writeTok(t, "add", 10<<20), upToken(t, upSecret, delClaims())
	del := func(p string) *httptest.ResponseRecorder { return send(ws, http.MethodDelete, p, delTok) }
	wantReply(t, "1", putPart(ws, upID, "Name/x/y/a", writeTok(t, "new", 10<<20), 0, 0, nil), 201, "") // a, x, x/y: 3
	wantReply(t, "2", del("Name/x/y/a"), 204, "")
	if fi, err := os.Stat(filepath.Join(files, "Name", "x", "y")); err != nil || !fi.IsDir() {
		t.Errorf("2: files/Name/x/y %v %v", fi, err)
	}
	wantReply(t, "3", putPart(ws, upID, "Name/x/y/b", addTok, 0, 0, nil), 201, "") // 2 dirs + the file
	wantReply(t, "3 DELETE", del("Name/x/y/b"), 204, "")
	wantReply(t, "4", putPart(ws, upID, "Name/q/c", addTok, 0, 0, nil), 413, "files") // 2 + q + the file
	if onDisk(filepath.Join(files, "Name", "q")) {
		t.Error("4: files/Name/q made")
	}
	wantReply(t, "5", del("Name/x/"), 204, "")
	if onDisk(filepath.Join(files, "Name", "x")) {
		t.Error("5: files/Name/x left")
	}
	wantReply(t, "6", putPart(ws, upID, "Name/q/c", addTok, 0, 0, nil), 201, "") // q + the file
}
