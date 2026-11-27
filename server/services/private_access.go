package services

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/anacrolix/torrent/metainfo"
)

// Reasons of a refusal (X-Seeder-Reason).
const (
	reasonPrivateDisabled  = "private_disabled"
	reasonAccountRequired  = "account_required"
	reasonNotEntitled      = "not_entitled"
	reasonBadRecord        = "bad_record"
	reasonBusy             = "busy"
	reasonStoreUnavailable = "store_unavailable"
)

// seederRefusal is why a private torrent is not served: 403, or 503 for a
// condition that passes.
type seederRefusal struct {
	status int    // 403 or 503
	reason string // X-Seeder-Reason
}

func (e *seederRefusal) Error() string { return "seeder refused: " + e.reason }

// writeRefusal answers a seederRefusal: its status, X-Seeder-Reason, and Retry-After: 5
// on a 503; the body is the reason. It reports false, writing nothing, for any other error.
func writeRefusal(w http.ResponseWriter, err error) bool {
	var r *seederRefusal
	if !errors.As(err, &r) {
		return false
	}
	w.Header().Set("X-Seeder-Reason", r.reason)
	if r.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	http.Error(w, r.reason, r.status)
	return true
}

var uidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validUID(uid string) bool { return uidRe.MatchString(uid) }

// metainfoPrivate reports whether mi's info is private (BEP 27).
func metainfoPrivate(mi *metainfo.MetaInfo) (bool, error) {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return false, err
	}
	return info.Private != nil && *info.Private, nil
}
