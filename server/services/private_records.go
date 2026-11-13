package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/webtor-io/lazymap"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PrivateRecord is the trackers torrent-store keeps for one (hash, uid), in
// plain text: the seeder has no key and decrypts nothing.
type PrivateRecord struct {
	Tiers [][]string // one tier per ts.Tier, URLs in order
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
		"ok, missing, unavailable (the store failed), bad (record unreadable or without trackers), " +
		"disabled (the store keeps no private records).",
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
	rec, outcome := recordFromReply(r, err)
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

func (s *PrivateRecords) ask(h, uid string) (*ts.GetPrivateRecordReply, error) {
	cl, err := s.ts.Get()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordLookupTimeout)
	defer cancel()
	return cl.GetPrivateRecord(ctx, &ts.GetPrivateRecordRequest{InfoHash: h, Uid: uid})
}

// recordFromReply maps the store's answer to an outcome. disabled and bad come
// only from the refusals named here; any other error is unavailable (fail
// closed), never "no record".
func recordFromReply(r *ts.GetPrivateRecordReply, err error) (*PrivateRecord, recordOutcome) {
	if err != nil {
		st := status.Convert(err)
		switch {
		case st.Code() == codes.Unimplemented: // a store from before private records
			return nil, recordDisabled
		case st.Code() == codes.FailedPrecondition && refusalReason(st) == "private_unavailable":
			return nil, recordDisabled
		case st.Code() == codes.DataLoss && refusalReason(st) == "record_unreadable":
			return nil, recordBad
		}
		return nil, recordUnavailable
	}
	if !r.GetFound() {
		return nil, recordMissing
	}
	rec := &PrivateRecord{}
	urls := 0
	for _, t := range r.GetTiers() {
		rec.Tiers = append(rec.Tiers, append([]string(nil), t.GetUrls()...))
		urls += len(t.GetUrls())
	}
	if urls == 0 {
		return nil, recordBad
	}
	return rec, recordOK
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
