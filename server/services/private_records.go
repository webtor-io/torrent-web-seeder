package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/webtor-io/lazymap"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PrivateRecord is the trackers torrent-store keeps for one (hash, uid), as its
// Pull with that uid returns them: the seeder has no key and decrypts nothing.
type PrivateRecord struct {
	Tiers [][]string // the reply's announce-list, or its announce as one tier
}

// recordOutcome is how a record lookup ended.
type recordOutcome string

const (
	recordOK          recordOutcome = "ok"
	recordMissing     recordOutcome = "missing"
	recordUnavailable recordOutcome = "unavailable"
	recordBad         recordOutcome = "bad"
	recordDisabled    recordOutcome = "disabled"
)

const recordCacheTTL = 60 * time.Second

var recordLookupTimeout = 10 * time.Second

var promRecordLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "torrent_web_seeder_private_record_lookups_total",
	Help: "Private record lookups answered by torrent-store (cache hits not counted), by outcome: " +
		"ok, missing, unavailable (the store failed), bad (reply unreadable, of another torrent or without trackers), " +
		"disabled (the store keeps no private records or does not return them by Pull).",
}, []string{"outcome"})

func init() {
	prometheus.MustRegister(promRecordLookups)
	for _, o := range []recordOutcome{recordOK, recordMissing, recordUnavailable, recordBad, recordDisabled} {
		promRecordLookups.WithLabelValues(string(o))
	}
}

// entryKey is the key of a loaded torrent and of a cached record: h for the
// default Client, h + "#" + uid for an account's.
func entryKey(h, uid string) string {
	if uid == "" {
		return h
	}
	return h + "#" + uid
}

// acct is the account as logs carry it: the first 8 hex digits of sha256(uid).
func acct(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:4])
}

// PrivateRecords reads (hash, uid) records from torrent-store. The cache is keyed
// entryKey(h, uid), Capacity 10000, Expire recordCacheTTL; it keeps
// ok|missing|bad|disabled and never unavailable: the loader returns that one as
// an error, which lazymap does not store.
type PrivateRecords struct {
	lazymap.LazyMap[*recordAnswer]
	ts *TorrentStore
}

type recordAnswer struct {
	rec     *PrivateRecord
	outcome recordOutcome
}

func NewPrivateRecords(ts *TorrentStore) *PrivateRecords {
	return &PrivateRecords{
		ts: ts,
		LazyMap: lazymap.New[*recordAnswer](&lazymap.Config{
			Capacity: 10000,
			Expire:   recordCacheTTL,
		}),
	}
}

// Get answers through the cache; a nil receiver answers (nil, recordDisabled).
func (s *PrivateRecords) Get(h, uid string) (*PrivateRecord, recordOutcome) {
	if s == nil {
		return nil, recordDisabled
	}
	a, err := s.LazyMap.Get(entryKey(h, uid), func() (*recordAnswer, error) { return s.lookup(h, uid) })
	if err != nil {
		return nil, recordUnavailable
	}
	return a.rec, a.outcome
}

// Refresh asks the store past the cache and leaves its answer there (Drop, then Get
// with a loader returning it); an unavailable answer leaves the cache empty.
//
// An equal answer already cached is left in place: lazymap's Drop stops the
// entry's expire timer, and the goroutine waiting on it never exits. Its
// cached copy then expires earlier than a fresh one would, at the cost of a
// store lookup, never a stale answer. A changed or unavailable answer still
// drops a live entry and leaks one goroutine per change; fixing lazymap's
// expiry (time.AfterFunc) removes that.
func (s *PrivateRecords) Refresh(h, uid string) (*PrivateRecord, recordOutcome) {
	if s == nil {
		return nil, recordDisabled
	}
	a, err := s.lookup(h, uid)
	key := entryKey(h, uid)
	if err != nil {
		s.Drop(key)
		return nil, recordUnavailable
	}
	load := func() (*recordAnswer, error) { return a, nil }
	if cur, err := s.LazyMap.Get(key, load); err != nil || !reflect.DeepEqual(cur, a) {
		s.Drop(key)
		_, _ = s.LazyMap.Get(key, load)
	}
	return a.rec, a.outcome
}

// lookup asks the store once, counts the answer and logs bad and unavailable
// by class only: no uid, URL or error text.
func (s *PrivateRecords) lookup(h, uid string) (*recordAnswer, error) {
	r, err := s.ask(h, uid)
	rec, outcome := recordFromReply(h, r, err)
	promRecordLookups.WithLabelValues(string(outcome)).Inc()
	if outcome == recordBad || outcome == recordUnavailable {
		log.WithFields(log.Fields{
			"infohash": h,
			"acct":     acct(uid),
			"outcome":  outcome,
			"code":     status.Code(err).String(),
		}).Warn("private record lookup failed")
	}
	if outcome == recordUnavailable { // only ever from an error, returned so lazymap does not keep it
		return nil, err
	}
	return &recordAnswer{rec: rec, outcome: outcome}, nil
}

// ask Pulls h as uid; a store that keeps records answers with uid's trackers.
func (s *PrivateRecords) ask(h, uid string) (*ts.PullReply, error) {
	cl, err := s.ts.Get()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordLookupTimeout)
	defer cancel()
	return cl.Pull(ctx, &ts.PullRequest{InfoHash: h, Caller: &ts.Caller{Uid: uid}})
}

// recordFromReply maps the store's answer to Pull(h, uid) to an outcome; only
// the trackers are kept, never the metainfo. disabled and missing come only
// from the answers named here; any other error is unavailable (fail closed),
// never "no record".
func recordFromReply(h string, r *ts.PullReply, err error) (*PrivateRecord, recordOutcome) {
	if err != nil {
		st := status.Convert(err)
		switch {
		case st.Code() == codes.PermissionDenied && refusalReason(st) == "no_record":
			return nil, recordMissing
		case st.Code() == codes.FailedPrecondition && refusalReason(st) == "private_unavailable":
			return nil, recordDisabled
		}
		return nil, recordUnavailable
	}
	// A store from before caller trackers answers with its stored copy, whose
	// trackers are the first uploader's (keep-first), not uid's.
	if !r.GetCallerTrackers() {
		return nil, recordDisabled
	}
	mi, err := metainfo.Load(bytes.NewReader(r.GetTorrent()))
	if err != nil || !strings.EqualFold(mi.HashInfoBytes().HexString(), h) {
		return nil, recordBad
	}
	rec := &PrivateRecord{Tiers: mi.UpvertedAnnounceList()}
	for _, t := range rec.Tiers {
		for _, u := range t {
			if u != "" {
				return rec, recordOK
			}
		}
	}
	return nil, recordBad
}

// refusalReason is the Reason of the status's ErrorInfo, "" if none. The
// Domain is not checked.
func refusalReason(st *status.Status) string {
	for _, d := range st.Details() {
		if e, ok := d.(*errdetails.ErrorInfo); ok {
			return e.GetReason()
		}
	}
	return ""
}
