package services

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
)

const maxPartBytes = 16 << 20 // 16 777 216
const maxPathDepth = 32       // path segments, the root and the file name included [estimate: real folders are 3–15 deep]

var (
	maxUploadFiles = 10000                                  // a test lowers it
	createHook     = func(step string) error { return nil } // before "touch", "dirs", "owner", "lock"
	walkHook       = func(stage string) {}                  // "parts": in reserved between parts/ and files/; "done": in newPart after reserved
	copyPart       = io.Copy                                // a test makes it hit ENOSPC
	walkFiles      = filepath.WalkDir                       // a test breaks it
)

var errManyRoots = errors.New("more than one dir in files/")

// upReply is a write's answer: its status, and for 409 and 413 the body.
type upReply struct {
	code int
	body string
}

func (e *upReply) Error() string { return fmt.Sprintf("%d %s", e.code, e.body) }

func reply(code int, body string) error { return &upReply{code: code, body: body} }

// writeReply answers err: *upReply as it is; errUploadGone 410; anything else
// is logged with h and answered 500 without a body.
func writeReply(w http.ResponseWriter, h string, err error) {
	var r *upReply
	switch {
	case errors.As(err, &r) && r.body == "":
		w.WriteHeader(r.code)
	case errors.As(err, &r):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(r.code)
		_, _ = io.WriteString(w, r.body)
	case errors.Is(err, errUploadGone):
		w.WriteHeader(http.StatusGone)
	default:
		log.WithError(err).WithField("hash", h).Error("upload write failed")
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// validPath answers whether p may be written: 2 to maxPathDepth
// segments, none empty, . or .., each up to 255 bytes, up to 1024 bytes in
// all, valid UTF-8 without ~ and control bytes. Without the UTF-8 rule the
// index's JSON would turn broken bytes into U+FFFD, and the manifest's path
// would no longer be the file's.
func validPath(p string) bool {
	if len(p) > 1024 || !utf8.ValidString(p) ||
		strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '~' }) {
		return false
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 || len(segs) > maxPathDepth {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return false
		}
	}
	return true
}

// options answers the browser's preflight of a write, without a token. THP
// sets Access-Control-Allow-Origin.
func (u *Upload) options(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "PUT, DELETE")
	h.Set("Access-Control-Allow-Headers", "content-type")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// put writes a part of file p of upload h: the body at ?offset of the file of
// ?size bytes, into parts/<sha1(p)>.<size>, moved to files/p once whole.
func (u *Upload) put(w http.ResponseWriter, r *http.Request, h, p string, t *writeClaims) {
	writeReply(w, h, u.writePart(r, h, p, t))
}

// writePart is put's work; its answer is the error, *upReply on success too.
// The order of the checks is the write contract's.
func (u *Upload) writePart(r *http.Request, h, p string, t *writeClaims) error {
	if !validPath(p) {
		return reply(http.StatusBadRequest, "")
	}
	q := r.URL.Query()
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		return reply(http.StatusBadRequest, "")
	}
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if err != nil || size < 0 {
		return reply(http.StatusBadRequest, "")
	}
	// Without a sum: neither offset nor size is bounded, offset+n would
	// overflow.
	n := r.ContentLength
	if n < 0 || n > maxPartBytes || offset > size || n > size-offset {
		return reply(http.StatusBadRequest, "")
	}
	dir, err := GetDir(u.ws.tom.p, h)
	if err != nil {
		return err
	}
	full, ok := uploadFile(dir, p)
	if !ok {
		return reply(http.StatusBadRequest, "")
	}
	lk, err := lockUpload(dir)
	if errors.Is(err, errUploadGone) && t.up == "new" {
		if err := u.create(dir, h, t.uid); err != nil {
			return err
		}
		lk, err = lockUpload(dir)
	}
	if err != nil {
		return err
	}
	defer func() { _ = lk.Close() }()
	if _, err := u.ws.tom.Touch(h); err != nil {
		log.WithError(err).WithField("hash", h).Error("upload touch failed")
	}
	name, err := uploadName(dir)
	if errors.Is(err, errManyRoots) {
		return reply(http.StatusConflict, "root")
	}
	if err != nil {
		return err
	}
	if root, _, _ := strings.Cut(p, "/"); name != "" && name != root {
		return reply(http.StatusConflict, "root")
	}
	part := filepath.Join(dir, "parts", fmt.Sprintf("%x.%d", sha1.Sum([]byte(p)), size))
	f, err := os.OpenFile(part, os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		f, err = newPart(dir, p, part, offset, size, t.lim)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// Held up to the rename: a second writer of the part gets its size.
	busy := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if busy != nil && !errors.Is(busy, syscall.EWOULDBLOCK) {
		return busy
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if busy != nil || fi.Size() != offset {
		return reply(http.StatusConflict, strconv.FormatInt(fi.Size(), 10))
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	written, err := copyPart(f, io.LimitReader(r.Body, n))
	if errors.Is(err, syscall.ENOSPC) {
		return reply(http.StatusInsufficientStorage, "")
	}
	if err != nil {
		return err
	}
	if offset+written < size {
		return reply(http.StatusNoContent, "")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	pd, err := lockParts(dir)
	if err != nil {
		return err
	}
	defer func() { _ = pd.Close() }()
	// The part's name brings no dirs of its own: the ones it makes now count.
	dirs, err := newDirs(dir, p)
	if err != nil {
		return err
	}
	if dirs > 0 {
		_, count, err := reserved(dir)
		if err != nil {
			return err
		}
		if count+dirs > maxUploadFiles { // the part itself is in count
			// Unlinked, then emptied by our own fd: its S leaves reserved.
			if err := os.Remove(part); err != nil {
				return err
			}
			if err := f.Truncate(0); err != nil {
				return err
			}
			return reply(http.StatusRequestEntityTooLarge, "files")
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.Rename(part, full); err != nil {
		return err
	}
	return reply(http.StatusCreated, "")
}

// create makes upload h's dir for uid (up=new): the touch first (the cleaner
// takes a dir without <id>.touch as the oldest), then files/ and parts/,
// owner, and .lock last: .lock makes the dir live, and it must not go live
// without an owner.
func (u *Upload) create(dir, h, uid string) error {
	if err := createHook("touch"); err != nil {
		return err
	}
	if _, err := u.ws.tom.touch(h); err != nil {
		return err
	}
	if err := createHook("dirs"); err != nil {
		return err
	}
	for _, d := range []string{"files", "parts"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	if err := createHook("owner"); err != nil {
		return err
	}
	o, err := os.OpenFile(filepath.Join(dir, "owner"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) { // another first file got here first; up=add will do
		return reply(http.StatusForbidden, "")
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(o, uid)
	if err == nil {
		err = o.Sync()
	}
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := createHook("lock"); err != nil {
		return err
	}
	lk, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_ = lk.Close()
	log.WithFields(log.Fields{"hash": h, "acct": acct(uid)}).Info("upload created")
	return nil
}

// lockParts takes parts/ of dir exclusive, waiting: a new part's check of lim
// and a part's move to files/ go one at a time.
func lockParts(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "parts"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if os.IsNotExist(err) {
		return nil, errUploadGone
	}
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// newPart makes the part of p, a file of size bytes, under the lock of
// parts/, once p is free and the ID has room for it.
func newPart(dir, p, part string, offset, size, lim int64) (*os.File, error) {
	pd, err := lockParts(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pd.Close() }()
	full, _ := uploadFile(dir, p)
	fi, err := os.Lstat(full)
	switch {
	case err == nil && fi.Mode().IsRegular() && fi.Size() == size:
		return nil, reply(http.StatusCreated, "")
	case err == nil:
		return nil, reply(http.StatusConflict, "exists")
	case errors.Is(err, syscall.ENOTDIR):
		// A file where a dir of p would be. Not "exists": the client would
		// rename the last segment with no end, and hit this again.
		return nil, reply(http.StatusBadRequest, "")
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	lines, err := deletedLines(dir)
	if err != nil {
		return nil, err
	}
	if inDeleted(lines, p) {
		return nil, reply(http.StatusConflict, "deleted")
	}
	ents, err := os.ReadDir(filepath.Dir(part))
	if err != nil {
		return nil, err
	}
	name := filepath.Base(part)
	sha := name[:strings.IndexByte(name, '.')+1]
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), sha) && e.Name() != name {
			return nil, reply(http.StatusConflict, "exists")
		}
	}
	if f, err := os.OpenFile(part, os.O_WRONLY, 0); err == nil { // made while this waited
		return f, nil
	}
	if offset != 0 {
		return nil, reply(http.StatusConflict, "0")
	}
	sum, count, err := reserved(dir)
	if err != nil {
		return nil, err
	}
	walkHook("done")
	// Without a sum: size is not bounded, sum+size would overflow.
	if size > lim || sum > lim-size {
		return nil, reply(http.StatusRequestEntityTooLarge, "size")
	}
	dirs, err := newDirs(dir, p)
	if err != nil {
		return nil, err
	}
	if count+dirs >= maxUploadFiles {
		return nil, reply(http.StatusRequestEntityTooLarge, "files")
	}
	return os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

// reserved is what dir holds against lim, Σ of files/ and Σ S of the parts/
// names, and count, its entries against maxUploadFiles: the parts, the
// regular files of files/ and its dirs below the root.
func reserved(dir string) (sum int64, count int, err error) {
	ents, err := os.ReadDir(filepath.Join(dir, "parts"))
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		sha, s, _ := strings.Cut(e.Name(), ".")
		size, err := strconv.ParseInt(s, 10, 64)
		if _, herr := hex.DecodeString(sha); len(sha) != 40 || herr != nil || err != nil || size < 0 {
			return 0, 0, fmt.Errorf("bad part name %q", e.Name())
		}
		sum += size
	}
	count = len(ents)
	walkHook("parts")
	files := filepath.Join(dir, "files")
	err = walkFiles(files, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			sum += fi.Size()
			count++
		} else if rel, _ := filepath.Rel(files, path); d.IsDir() && strings.ContainsRune(rel, filepath.Separator) {
			count++ // not files/ itself, nor the root
		}
		return nil
	})
	return sum, count, err
}

// newDirs counts the dirs below the root that MkdirAll for p would make.
func newDirs(dir, p string) (int, error) {
	segs := strings.Split(p, "/")
	n := 0
	for i := len(segs) - 1; i >= 2; i-- {
		_, err := os.Lstat(filepath.Join(dir, "files", filepath.Join(segs[:i]...)))
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		n++
	}
	return n, nil
}

// uploadName is the upload's root: "" while files/ is empty, its one dir, or
// errManyRoots.
func uploadName(dir string) (string, error) {
	ents, err := os.ReadDir(filepath.Join(dir, "files"))
	if err != nil {
		return "", err
	}
	name := ""
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if name != "" {
			return "", errManyRoots
		}
		name = e.Name()
	}
	return name, nil
}

// deletedLines reads dir/deleted, a path per line; no file → nil.
func deletedLines(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "deleted"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' }), nil
}

// inDeleted answers whether p, or a dir above it, is a line of deleted.
func inDeleted(lines []string, p string) bool {
	for _, l := range lines {
		if p == l || strings.HasPrefix(p, l+"/") {
			return true
		}
	}
	return false
}
