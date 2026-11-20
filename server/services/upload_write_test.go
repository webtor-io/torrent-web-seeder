package services

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
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
