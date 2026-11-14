package services

import (
	"bytes"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus/testutil"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordStore is a storeFake holding one private torrent, announcing
// keyStored as the first uploader's copy does, and another one; it returns the
// first's info hash and the second's.
func recordStore(t *testing.T) (f *storeFake, h, other string) {
	t.Helper()
	_, mi := privatePayload(t, "http://tracker.invalid/announce?passkey="+keyStored)
	_, mo := privatePayload(t)
	h, other = mi.HashInfoBytes().HexString(), mo.HashInfoBytes().HexString()
	return &storeFake{torrents: map[string][]byte{h: torrentBytes(t, mi), other: torrentBytes(t, mo)}}, h, other
}

func TestPrivateRecords_Outcomes(t *testing.T) {
	url := "http://tracker.invalid/announce?passkey=" + keyA
	url2 := "http://tracker.invalid/announce?passkey=" + keyA2
	type reply func(f *storeFake, h, other string) (*ts.PullReply, error)
	tiers := func(tt ...[]string) reply {
		return func(f *storeFake, h, _ string) (*ts.PullReply, error) { return f.callerReply(h, tt...) }
	}
	fails := func(err error) reply {
		return func(*storeFake, string, string) (*ts.PullReply, error) { return nil, err }
	}
	for _, c := range []struct {
		name  string
		reply reply
		want  recordOutcome
		tiers [][]string
	}{
		{"record", tiers([]string{url}), recordOK, [][]string{{url}}},
		{"record of two tiers", tiers([]string{url}, []string{url2}), recordOK, [][]string{{url}, {url2}}},
		{"announce only", func(f *storeFake, h, _ string) (*ts.PullReply, error) {
			r, err := f.callerReply(h, []string{url})
			if err != nil {
				return nil, err
			}
			mi, err := metainfo.Load(bytes.NewReader(r.Torrent))
			if err != nil {
				return nil, err
			}
			mi.AnnounceList = nil
			r.Torrent = torrentBytes(t, mi)
			return r, nil
		}, recordOK, [][]string{{url}}},
		{"no record", fails(storeErr(codes.PermissionDenied, "no_record")), recordMissing, nil},
		{"no S3 or key", fails(storeErr(codes.FailedPrecondition, "private_unavailable")), recordDisabled, nil},
		// A store from before caller trackers answers a uid with its stored
		// copy, the first uploader's trackers in it, and no flag.
		{"old store", func(f *storeFake, h, _ string) (*ts.PullReply, error) { return f.stored(h) }, recordDisabled, nil},
		{"old store, the record's trackers", func(f *storeFake, h, _ string) (*ts.PullReply, error) {
			r, err := f.callerReply(h, []string{url})
			if r != nil {
				r.CallerTrackers = false
			}
			return r, err
		}, recordDisabled, nil},
		{"record without trackers", tiers(), recordBad, nil},
		{"record of empty URLs", tiers([]string{""}), recordBad, nil},
		{"unparseable", func(*storeFake, string, string) (*ts.PullReply, error) {
			return &ts.PullReply{Torrent: []byte("not bencode"), CallerTrackers: true}, nil
		}, recordBad, nil},
		{"another torrent", func(f *storeFake, _, other string) (*ts.PullReply, error) {
			return f.callerReply(other, []string{url})
		}, recordBad, nil},
		{"store down", fails(status.Error(codes.Unavailable, "down")), recordUnavailable, nil},
		{"denied for another reason", fails(storeErr(codes.PermissionDenied, "sign_in")), recordUnavailable, nil},
		{"denied without a reason", fails(status.Error(codes.PermissionDenied, "x")), recordUnavailable, nil},
		{"precondition of another kind", fails(status.Error(codes.FailedPrecondition, "x")), recordUnavailable, nil},
		{"torrent not in the store", fails(status.Error(codes.NotFound, "x")), recordUnavailable, nil},
		{"unimplemented", fails(status.Error(codes.Unimplemented, "x")), recordUnavailable, nil},
		{"timeout", fails(status.Error(codes.DeadlineExceeded, "slow")), recordUnavailable, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, h, other := recordStore(t)
			f.setRecord(func(in *ts.PullRequest) (*ts.PullReply, error) {
				if in.GetInfoHash() != h || in.GetCaller().GetUid() != uidA || in.GetCaller().GetInternal() {
					t.Errorf("asked (%s, %v), want (%s, uid %s)", in.GetInfoHash(), in.GetCaller(), h, uidA)
				}
				return c.reply(f, h, other)
			})
			before := testutil.ToFloat64(promRecordLookups.WithLabelValues(string(c.want)))
			rec, got := NewPrivateRecords(f.store()).Get(h, uidA)
			if got != c.want {
				t.Fatalf("outcome %s, want %s", got, c.want)
			}
			if d := testutil.ToFloat64(promRecordLookups.WithLabelValues(string(c.want))) - before; d != 1 {
				t.Fatalf("lookups{%s} +%v, want +1", c.want, d)
			}
			if c.want == recordOK && (rec == nil || !reflect.DeepEqual(rec.Tiers, c.tiers)) {
				t.Fatalf("record %+v, want tiers %v", rec, c.tiers)
			}
			if c.want != recordOK && rec != nil {
				t.Fatalf("record %+v on %s, want nil", rec, c.want)
			}
			if n := f.lookupCount(); n != 1 || len(f.pullRequests()) != 1 {
				t.Fatalf("lookups %d, pulls %d, want one Pull", n, len(f.pullRequests()))
			}
		})
	}
	if acct(uidA) != "deb87fab" || acct(uidB) != "5f44da3a" {
		t.Fatalf("acct = %s, %s", acct(uidA), acct(uidB))
	}
}

// The store lowercases the info hash it is asked for; a reply to an upper-case
// one is still of the same torrent.
func TestPrivateRecords_HashCase(t *testing.T) {
	f, h, _ := recordStore(t)
	f.setRecord(func(in *ts.PullRequest) (*ts.PullReply, error) {
		return f.callerReply(strings.ToLower(in.GetInfoHash()), []string{"http://tracker.invalid/a"})
	})
	if _, o := NewPrivateRecords(f.store()).Get(strings.ToUpper(h), uidA); o != recordOK {
		t.Fatalf("outcome %s, want ok", o)
	}
}

func TestPrivateRecords_CacheAndRefresh(t *testing.T) {
	f, h, _ := recordStore(t)
	f.setTiers([]string{"http://tracker.invalid/a"})
	pr := NewPrivateRecords(f.store())
	pr.Get(h, uidA)
	pr.Get(h, uidA)
	pr.Get(h, uidB)
	if n := f.lookupCount(); n != 2 {
		t.Fatalf("store asked %d times, want 2 (one per pair, then the cache)", n)
	}
	f.setTiers([]string{"http://tracker.invalid/b"})
	if rec, _ := pr.Refresh(h, uidA); rec.Tiers[0][0] != "http://tracker.invalid/b" || f.lookupCount() != 3 {
		t.Fatalf("Refresh = %+v after %d lookups, want the new tracker from the store", rec, f.lookupCount())
	}
	if rec, _ := pr.Get(h, uidA); rec.Tiers[0][0] != "http://tracker.invalid/b" || f.lookupCount() != 3 {
		t.Fatalf("Get after Refresh = %+v after %d lookups, want the new tracker from the cache", rec, f.lookupCount())
	}
	f.setRecord(func(*ts.PullRequest) (*ts.PullReply, error) {
		return nil, status.Error(codes.Unavailable, "down")
	})
	pr.Get("0000000000000000000000000000000000000001", uidA)
	pr.Get("0000000000000000000000000000000000000001", uidA)
	if n := f.lookupCount(); n != 5 {
		t.Fatalf("lookups %d, want 5: an unavailable store is asked again, not cached", n)
	}
	f.setRecord(func(*ts.PullRequest) (*ts.PullReply, error) {
		return nil, storeErr(codes.PermissionDenied, "no_record")
	})
	pr.Get("0000000000000000000000000000000000000002", uidA)
	pr.Get("0000000000000000000000000000000000000002", uidA)
	if n := f.lookupCount(); n != 6 {
		t.Fatalf("lookups %d, want 6: missing is cached", n)
	}
	f.setRecord(func(*ts.PullRequest) (*ts.PullReply, error) {
		return nil, status.Error(codes.Unavailable, "down")
	})
	if rec, o := pr.Refresh(h, uidA); o != recordUnavailable || rec != nil {
		t.Fatalf("Refresh with the store down = %+v, %s, want nil, unavailable", rec, o)
	}
	f.setTiers([]string{"http://tracker.invalid/c"})
	if rec, _ := pr.Get(h, uidA); rec == nil || rec.Tiers[0][0] != "http://tracker.invalid/c" || f.lookupCount() != 8 {
		t.Fatalf("Get after an unavailable Refresh = %+v after %d lookups, want the store asked again (8)", rec, f.lookupCount())
	}
}

// lazymap's Drop leaves the dropped entry's expire goroutine blocked for good,
// so a Refresh of an unchanged record must not drop it.
func TestPrivateRecords_RefreshUnchangedLeaksNoGoroutine(t *testing.T) {
	f, h, _ := recordStore(t)
	f.setTiers([]string{"http://tracker.invalid/a"})
	pr := NewPrivateRecords(f.store())
	pr.Get(h, uidA)
	before := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		if rec, o := pr.Refresh(h, uidA); o != recordOK || rec.Tiers[0][0] != "http://tracker.invalid/a" {
			t.Fatalf("Refresh = %+v, %s", rec, o)
		}
	}
	waitFor(t, "100 Refreshes of an unchanged record to leave the goroutines as they were", func() bool {
		return runtime.NumGoroutine() <= before+5
	})
}

// A store that never answers gives unavailable at recordLookupTimeout.
func TestPrivateRecords_LookupTimeout(t *testing.T) {
	old := recordLookupTimeout
	recordLookupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { recordLookupTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f, h, _ := recordStore(t)
	f.setRecord(func(in *ts.PullRequest) (*ts.PullReply, error) {
		<-release
		return f.callerReply(in.GetInfoHash(), []string{"http://tracker.invalid/a"})
	})
	done := make(chan recordOutcome, 1)
	go func() {
		_, o := NewPrivateRecords(f.store()).Get(h, uidA)
		done <- o
	}()
	select {
	case o := <-done:
		if o != recordUnavailable {
			t.Fatalf("outcome %s, want unavailable", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the lookup outlived recordLookupTimeout")
	}
}

// No client from TorrentStore.Get() is a store failure: unavailable, never missing.
func TestPrivateRecords_NoStoreClient(t *testing.T) {
	before := testutil.ToFloat64(promRecordLookups.WithLabelValues(string(recordUnavailable)))
	store := &TorrentStore{inited: true, err: errors.New("dial failed")}
	if rec, o := NewPrivateRecords(store).Get("f07c04a783450dba366e562c253ce56573d7535f", uidA); o != recordUnavailable || rec != nil {
		t.Fatalf("Get = %+v, %s, want nil, unavailable", rec, o)
	}
	if d := testutil.ToFloat64(promRecordLookups.WithLabelValues(string(recordUnavailable))) - before; d != 1 {
		t.Fatalf("lookups{unavailable} +%v, want +1", d)
	}
}
