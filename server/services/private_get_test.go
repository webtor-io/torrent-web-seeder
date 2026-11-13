package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus/testutil"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGet_PublicGoesToDefaultClient(t *testing.T) {
	env := newPrivateEnv(t, true)
	for _, uid := range []string{"", uidA} {
		tor, key, err := env.tm.Get(context.Background(), env.pub, uid)
		if err != nil || tor == nil || key != env.pub {
			t.Fatalf("uid %q: (%v, %q, %v), want the default Client under %s", uid, tor, key, err, env.pub)
		}
	}
	if len(env.tm.accounts) != 0 || env.store.lookupCount() != 0 {
		t.Fatalf("accounts %d, record lookups %d, want 0 and 0", len(env.tm.accounts), env.store.lookupCount())
	}
}

func TestGet_PrivateWithRecordAnnouncesOwnKey(t *testing.T) {
	env := newPrivateEnv(t, false)
	env.setRecords(testRecord{uidA, keyA})
	tor, key, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil || key != env.h+"#"+uidA {
		t.Fatalf("(%q, %v), want key %s#%s", key, err, env.h, uidA)
	}
	if mi := tor.Metainfo(); !reflect.DeepEqual(mi.UpvertedAnnounceList(), metainfo.AnnounceList{{env.trk.URL(keyA)}}) {
		t.Fatalf("trackers %v, want only the record's", mi.UpvertedAnnounceList())
	}
	env.trk.waitEvent(t, keyA, "started")
	time.Sleep(500 * time.Millisecond)
	if n := len(env.trk.Announces(keyStored)); n != 0 {
		t.Fatalf("the stored key was announced %d times", n)
	}
	if _, ok := env.defaultClient(t).Torrent(tor.InfoHash()); ok {
		t.Fatal("the private torrent is in the default Client")
	}
}

func TestGet_PrivateRefusals(t *testing.T) {
	replies := func(r *ts.GetPrivateRecordReply) func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) { return r, nil }
	}
	fails := func(err error) func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) { return nil, err }
	}
	cases := []struct {
		name, uid string
		record    func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error)
		status    int
		reason    string
		both      bool // refused in observe too
	}{
		{"no uid", "", nil, 403, reasonAccountRequired, false},
		{"malformed uid", strings.ToUpper(uidA), nil, 403, reasonAccountRequired, true},
		{"no record", uidA, replies(&ts.GetPrivateRecordReply{}), 403, reasonNotEntitled, false},
		{"store without S3 or key", uidA, fails(storeErr(codes.FailedPrecondition, "private_unavailable")), 403, reasonPrivateDisabled, false},
		{"old store", uidA, nil /* storeFake: Unimplemented */, 403, reasonPrivateDisabled, false},
		{"unreadable record", uidA, fails(storeErr(codes.DataLoss, "record_unreadable")), 403, reasonBadRecord, false},
		{"record without trackers", uidA, replies(&ts.GetPrivateRecordReply{Found: true}), 403, reasonBadRecord, false},
		{"store down", uidA, fails(status.Error(codes.Unavailable, "down")), 503, reasonStoreUnavailable, true},
	}
	for _, c := range cases {
		for _, enforce := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enforce=%v", c.name, enforce), func(t *testing.T) {
				env := newPrivateEnv(t, enforce)
				env.store.setRecord(c.record)
				if want := map[bool]float64{true: 1}[enforce]; testutil.ToFloat64(promPrivateEnforce) != want {
					t.Fatalf("private_enforce = %v, want %v", testutil.ToFloat64(promPrivateEnforce), want)
				}
				if enforce || c.both {
					_, _, err := env.tm.Get(context.Background(), env.h, c.uid)
					var ref *seederRefusal
					if !errors.As(err, &ref) || ref.status != c.status || ref.reason != c.reason {
						t.Fatalf("err = %v, want %d %s", err, c.status, c.reason)
					}
					if n := len(env.defaultClient(t).Torrents()) + env.accountTorrents(); n != 0 {
						t.Fatalf("%d torrents loaded after a refusal, want 0", n)
					}
					return
				}
				fb := testutil.ToFloat64(promPrivateFallback.WithLabelValues(c.reason))
				tor, key, err := env.tm.Get(context.Background(), env.h, c.uid)
				if err != nil || tor == nil || key != env.h {
					t.Fatalf("observe: (%v, %q, %v), want today's path under %s", tor, key, err, env.h)
				}
				if d := testutil.ToFloat64(promPrivateFallback.WithLabelValues(c.reason)) - fb; d != 1 {
					t.Fatalf("fallback{%s} +%v, want +1", c.reason, d)
				}
			})
		}
	}
}

// A pair already loaded is served without asking the store: neither its outage nor
// the end of the 60 s cache cuts a running stream.
func TestGet_LoadedPairSurvivesStoreOutage(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	first, _, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil {
		t.Fatal(err)
	}
	env.store.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return nil, status.Error(codes.Unavailable, "down")
	})
	env.tm.records.Drop(entryKey(env.h, uidA))
	asked := env.store.lookupCount()
	again, _, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil || again != first || env.store.lookupCount() != asked {
		t.Fatalf("(%v, %v), lookups +%d: want the loaded torrent, no lookup", again == first, err, env.store.lookupCount()-asked)
	}
}

// Review Focus 1.
func TestGet_SlowRecordLookupDoesNotBlockPublic(t *testing.T) {
	env := newPrivateEnv(t, true)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	env.store.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		<-release
		return &ts.GetPrivateRecordReply{}, nil
	})
	go env.tm.Get(context.Background(), env.h, uidA)
	waitFor(t, "the record lookup to start", func() bool { return env.store.lookupCount() == 1 })
	done := make(chan error, 1)
	go func() { _, _, err := env.tm.Get(context.Background(), env.pub, ""); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a public Get waited on another torrent's record lookup")
	}
}

// Review Focus 2.
func TestGet_ConcurrentFirstRequestsTrackOnce(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	// A slow lookup: every request is past the first check when the answer
	// comes, so all of them race for the second.
	found := env.store.record
	env.store.setRecord(func(in *ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		time.Sleep(200 * time.Millisecond)
		return found(in)
	})
	active := testutil.ToFloat64(promActiveTorrentCount)
	tors := make([]*torrent.Torrent, 8)
	var wg sync.WaitGroup
	for i := range tors {
		wg.Add(1)
		go func() { defer wg.Done(); tors[i], _, _ = env.tm.Get(context.Background(), env.h, uidA) }()
	}
	wg.Wait()
	for i, tor := range tors {
		if tor == nil || tor != tors[0] {
			t.Fatalf("request %d got %v, want the one torrent", i, tor)
		}
	}
	if d := testutil.ToFloat64(promActiveTorrentCount) - active; d != 1 || len(env.tm.accounts) != 1 {
		t.Fatalf("active torrents +%v, accounts %d, want +1 and 1", d, len(env.tm.accounts))
	}
}
