package services

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/webtor-io/torrent-web-seeder/server/services/uploads"
)

// uploadManifest is an upload's index, GET /<id>/.
type uploadManifest struct {
	Kind     string      `json:"kind,omitempty"`     // "upload" for an owned upload
	Name     string      `json:"name"`               // the root; "" while the first file is a part
	UIDs     []string    `json:"uids"`               // never null
	Reserved *int64      `json:"reserved,omitempty"` // absent when listed by Vault
	Files    []VaultFile `json:"files"`              // never null; path without a leading "/"
}

// index serves upload h's manifest: from its dir while the dir lives, else
// from Vault. It does not touch the dir: the manifest is read before a view is
// allowed, and refused views would keep the upload alive.
func (u *Upload) index(w http.ResponseWriter, r *http.Request, h string) {
	m, err := u.manifest(r.Context(), h)
	switch {
	case err != nil: // never a part of the list
		log.WithError(err).WithField("hash", h).Error("upload index failed")
		w.WriteHeader(http.StatusInternalServerError)
	case m == nil:
		http.NotFound(w, r)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	}
}

// manifest reads upload h's manifest; nil when there is none. The locks are
// gone once it returns: a slow reader of the answer holds no writer back.
func (u *Upload) manifest(ctx context.Context, h string) (*uploadManifest, error) {
	dir, err := GetDir(u.ws.tom.p, h)
	if err != nil {
		return nil, err
	}
	lk, err := lockUpload(dir)
	if errors.Is(err, errUploadGone) {
		return u.vaultManifest(ctx, h)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = lk.Close() }()
	if gone, err := tombstoned(dir); err != nil || gone { // deleted: no manifest, not Vault's either
		return nil, err
	}
	// Under the lock of parts/, as a part's move to files/: a move between
	// the read of parts/ and the walk of files/ would count its S twice.
	pd, err := lockParts(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pd.Close() }()
	name, err := uploadName(dir)
	if err != nil {
		return nil, err
	}
	sum, _, err := reserved(dir)
	if err != nil {
		return nil, err
	}
	files := filepath.Join(dir, "files")
	list := []VaultFile{}
	err = walkFiles(files, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(files, path)
		if err != nil {
			return err
		}
		list = append(list, VaultFile{Path: filepath.ToSlash(rel), Size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &uploadManifest{Kind: manifestKind(h), Name: name, UIDs: readOwner(dir), Reserved: &sum, Files: list}, nil
}

// vaultManifest is the manifest of evicted upload h, as Vault lists it; nil
// when Vault has none or is not set.
func (u *Upload) vaultManifest(ctx context.Context, h string) (*uploadManifest, error) {
	if u.ws.v == nil {
		return nil, nil
	}
	l, err := u.ws.v.ListFiles(ctx, h)
	if err != nil || l == nil {
		return nil, err
	}
	m := &uploadManifest{Kind: manifestKind(h), UIDs: l.UIDs, Files: l.Files}
	if m.UIDs == nil {
		m.UIDs = []string{}
	}
	if m.Files == nil {
		m.Files = []VaultFile{}
	}
	for i := range m.Files {
		m.Files[i].Path = strings.TrimPrefix(m.Files[i].Path, "/")
	}
	if len(m.Files) > 0 {
		m.Name, _, _ = strings.Cut(m.Files[0].Path, "/")
	}
	return m, nil
}

// manifestKind is the manifest's kind of h: "upload" for an owned upload.
func manifestKind(h string) string {
	if uploads.Owned(h) {
		return "upload"
	}
	return ""
}

// readOwner is [uid] when dir/owner holds one; [] otherwise (missing, broken).
func readOwner(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "owner"))
	if err != nil || !validUID(string(b)) {
		return []string{}
	}
	return []string{string(b)}
}
