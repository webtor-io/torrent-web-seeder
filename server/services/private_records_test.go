package services

import (
	"errors"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordReply is a found record with one tier of urls.
func recordReply(urls ...string) *ts.GetPrivateRecordReply {
	return &ts.GetPrivateRecordReply{Found: true, Tiers: []*ts.Tier{{Urls: urls}}}
}

// storeErr is a refusal as torrent-store sends it: code with ErrorInfo{Reason}.
func storeErr(code codes.Code, reason string) error {
	st, err := status.New(code, "refused").WithDetails(&errdetails.ErrorInfo{Domain: "torrent-store", Reason: reason})
	if err != nil {
		panic(err)
	}
	return st.Err()
}

func TestPrivateRecords_Outcomes(t *testing.T) {
	const h = "f07c04a783450dba366e562c253ce56573d7535f"
	url := "http://tracker.invalid/announce?passkey=" + keyA
	for _, c := range []struct {
		name  string
		reply *ts.GetPrivateRecordReply
		err   error
		want  recordOutcome
	}{
		{"found", recordReply(url), nil, recordOK},
		{"not found", &ts.GetPrivateRecordReply{}, nil, recordMissing},
		{"found without trackers", &ts.GetPrivateRecordReply{Found: true}, nil, recordBad},
		{"no S3 or key", nil, storeErr(codes.FailedPrecondition, "private_unavailable"), recordDisabled},
		{"old store", nil, status.Error(codes.Unimplemented, "unknown method"), recordDisabled},
		{"unreadable", nil, storeErr(codes.DataLoss, "record_unreadable"), recordBad},
		{"store down", nil, status.Error(codes.Unavailable, "down"), recordUnavailable},
		{"precondition of another kind", nil, status.Error(codes.FailedPrecondition, "x"), recordUnavailable},
		{"data loss of another kind", nil, status.Error(codes.DataLoss, "x"), recordUnavailable},
		{"timeout", nil, status.Error(codes.DeadlineExceeded, "slow"), recordUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &storeFake{}
			f.setRecord(func(in *ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
				if in.GetInfoHash() != h || in.GetUid() != uidA {
					t.Errorf("asked (%s, %s), want (%s, %s)", in.GetInfoHash(), in.GetUid(), h, uidA)
				}
				return c.reply, c.err
			})
			before := testutil.ToFloat64(promRecordLookups.WithLabelValues(string(c.want)))
			rec, got := NewPrivateRecords(f.store()).Get(h, uidA)
			if got != c.want {
				t.Fatalf("outcome %s, want %s", got, c.want)
			}
			if d := testutil.ToFloat64(promRecordLookups.WithLabelValues(string(c.want))) - before; d != 1 {
				t.Fatalf("lookups{%s} +%v, want +1", c.want, d)
			}
			if c.want == recordOK && (rec == nil || !reflect.DeepEqual(rec.Tiers, [][]string{{url}})) {
				t.Fatalf("record %+v", rec)
			}
			if c.want != recordOK && rec != nil {
				t.Fatalf("record %+v on %s, want nil", rec, c.want)
			}
		})
	}
	if acct(uidA) != "deb87fab" || acct(uidB) != "5f44da3a" {
		t.Fatalf("acct = %s, %s", acct(uidA), acct(uidB))
	}
}

func TestPrivateRecords_CacheAndRefresh(t *testing.T) {
	const h = "f07c04a783450dba366e562c253ce56573d7535f"
	f := &storeFake{}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return recordReply("http://tracker.invalid/a"), nil
	})
	pr := NewPrivateRecords(f.store())
	pr.Get(h, uidA)
	pr.Get(h, uidA)
	pr.Get(h, uidB)
	if n := f.lookupCount(); n != 2 {
		t.Fatalf("store asked %d times, want 2 (one per pair, then the cache)", n)
	}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return recordReply("http://tracker.invalid/b"), nil
	})
	if rec, _ := pr.Refresh(h, uidA); rec.Tiers[0][0] != "http://tracker.invalid/b" || f.lookupCount() != 3 {
		t.Fatalf("Refresh = %+v after %d lookups, want the new tracker from the store", rec, f.lookupCount())
	}
	if rec, _ := pr.Get(h, uidA); rec.Tiers[0][0] != "http://tracker.invalid/b" || f.lookupCount() != 3 {
		t.Fatalf("Get after Refresh = %+v after %d lookups, want the new tracker from the cache", rec, f.lookupCount())
	}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return nil, status.Error(codes.Unavailable, "down")
	})
	pr.Get("0000000000000000000000000000000000000001", uidA)
	pr.Get("0000000000000000000000000000000000000001", uidA)
	if n := f.lookupCount(); n != 5 {
		t.Fatalf("lookups %d, want 5: an unavailable store is asked again, not cached", n)
	}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return &ts.GetPrivateRecordReply{}, nil
	})
	pr.Get("0000000000000000000000000000000000000002", uidA)
	pr.Get("0000000000000000000000000000000000000002", uidA)
	if n := f.lookupCount(); n != 6 {
		t.Fatalf("lookups %d, want 6: missing is cached", n)
	}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return nil, status.Error(codes.Unavailable, "down")
	})
	if rec, o := pr.Refresh(h, uidA); o != recordUnavailable || rec != nil {
		t.Fatalf("Refresh with the store down = %+v, %s, want nil, unavailable", rec, o)
	}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return recordReply("http://tracker.invalid/c"), nil
	})
	if rec, _ := pr.Get(h, uidA); rec == nil || rec.Tiers[0][0] != "http://tracker.invalid/c" || f.lookupCount() != 8 {
		t.Fatalf("Get after an unavailable Refresh = %+v after %d lookups, want the store asked again (8)", rec, f.lookupCount())
	}
}

// lazymap's Drop leaves the dropped entry's expire goroutine blocked for good,
// so a Refresh of an unchanged record must not drop it.
func TestPrivateRecords_RefreshUnchangedLeaksNoGoroutine(t *testing.T) {
	const h = "f07c04a783450dba366e562c253ce56573d7535f"
	f := &storeFake{}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return recordReply("http://tracker.invalid/a"), nil
	})
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

// Р4: a store that never answers gives unavailable at recordLookupTimeout.
func TestPrivateRecords_LookupTimeout(t *testing.T) {
	old := recordLookupTimeout
	recordLookupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { recordLookupTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f := &storeFake{}
	f.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		<-release
		return recordReply("http://tracker.invalid/a"), nil
	})
	done := make(chan recordOutcome, 1)
	go func() {
		_, o := NewPrivateRecords(f.store()).Get("f07c04a783450dba366e562c253ce56573d7535f", uidA)
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
