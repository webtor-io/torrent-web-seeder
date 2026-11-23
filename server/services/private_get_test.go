package services

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGet_PublicGoesToDefaultClient(t *testing.T) {
	env := newPrivateEnv(t)
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
	env := newPrivateEnv(t)
	env.setRecords(testRecord{uidA, keyA})
	// The stored copy names a web seed (a closed loopback port), and the
	// Clients take web seeds: the account's torrent must still have none.
	mi, err := metainfo.Load(bytes.NewReader(env.store.torrents[env.h]))
	if err != nil {
		t.Fatal(err)
	}
	mi.UrlList = []string{"http://127.0.0.1:1/webseed"}
	env.store.torrents[env.h] = torrentBytes(t, mi)
	env.tm.tc.testConfig = func(c *torrent.ClientConfig) {
		trackerTestConfig(c)
		c.DisableWebseeds = false
	}
	tor, key, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil || key != env.h+"#"+uidA {
		t.Fatalf("(%q, %v), want key %s#%s", key, err, env.h, uidA)
	}
	if mi := tor.Metainfo(); !reflect.DeepEqual(mi.UpvertedAnnounceList(), metainfo.AnnounceList{{env.trk.URL(keyA)}}) {
		t.Fatalf("trackers %v, want only the record's", mi.UpvertedAnnounceList())
	}
	if ul := tor.Metainfo().UrlList; len(ul) != 0 {
		t.Fatalf("web seeds %v, want none", ul)
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

// A store from before caller trackers answers a uid's Pull with a torrent
// whose trackers are not that uid's record's (keep-first's), without
// caller_trackers. The seeder takes no tracker from such a reply and refuses
// private_disabled. Here the reply names the record's key, so taking it would
// announce that key.
func TestGet_ReplyWithoutCallerTrackersIsNotARecord(t *testing.T) {
	env := newPrivateEnv(t)
	env.store.setRecord(func(in *ts.PullRequest) (*ts.PullReply, error) {
		r, err := env.store.callerReply(in.GetInfoHash(), []string{env.trk.URL(keyA)})
		if r != nil {
			r.CallerTrackers = false
		}
		return r, err
	})
	_, _, err := env.tm.Get(context.Background(), env.h, uidA)
	if env.store.lookupCount() != 1 {
		t.Fatalf("lookups %d, want 1", env.store.lookupCount())
	}
	var ref *seederRefusal
	if !errors.As(err, &ref) || ref.reason != reasonPrivateDisabled {
		t.Fatalf("err = %v, want private_disabled", err)
	}
	if n := len(env.defaultClient(t).Torrents()) + env.accountTorrents(); n != 0 {
		t.Fatalf("%d torrents loaded after a refusal, want 0", n)
	}
	if n := len(env.trk.Announces(keyA)); n != 0 {
		t.Fatalf("the reply's key was announced %d times", n)
	}
}

// Every refusal is answered, whatever the deployment: no refusal loads the
// private torrent anywhere, so the stored copy's trackers are never announced.
func TestGet_PrivateRefusals(t *testing.T) {
	type record func(env *privateEnv) func(*ts.PullRequest) (*ts.PullReply, error)
	fails := func(err error) record {
		return func(*privateEnv) func(*ts.PullRequest) (*ts.PullReply, error) {
			return func(*ts.PullRequest) (*ts.PullReply, error) { return nil, err }
		}
	}
	replies := func(fn func(env *privateEnv, in *ts.PullRequest) (*ts.PullReply, error)) record {
		return func(env *privateEnv) func(*ts.PullRequest) (*ts.PullReply, error) {
			return func(in *ts.PullRequest) (*ts.PullReply, error) { return fn(env, in) }
		}
	}
	none := func(*privateEnv) func(*ts.PullRequest) (*ts.PullReply, error) { return nil }
	cases := []struct {
		name, uid string
		record    record
		status    int
		reason    string
	}{
		{"no uid", "", none, 403, reasonAccountRequired},
		{"malformed uid", strings.ToUpper(uidA), none, 403, reasonAccountRequired},
		{"no record", uidA, fails(storeErr(codes.PermissionDenied, "no_record")), 403, reasonNotEntitled},
		{"store without S3 or key", uidA, fails(storeErr(codes.FailedPrecondition, "private_unavailable")), 403, reasonPrivateDisabled},
		{"old store", uidA, none /* storeFake: the stored copy, no flag */, 403, reasonPrivateDisabled},
		{"unparseable reply", uidA, replies(func(*privateEnv, *ts.PullRequest) (*ts.PullReply, error) {
			return &ts.PullReply{Torrent: []byte("not bencode"), CallerTrackers: true}, nil
		}), 403, reasonBadRecord},
		{"reply of another torrent", uidA, replies(func(env *privateEnv, _ *ts.PullRequest) (*ts.PullReply, error) {
			return env.store.callerReply(env.pub, []string{env.trk.URL(keyA)})
		}), 403, reasonBadRecord},
		{"record without trackers", uidA, replies(func(env *privateEnv, in *ts.PullRequest) (*ts.PullReply, error) {
			return env.store.callerReply(in.GetInfoHash())
		}), 403, reasonBadRecord},
		{"store down", uidA, fails(status.Error(codes.Unavailable, "down")), 503, reasonStoreUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newPrivateEnv(t)
			env.store.setRecord(c.record(env))
			_, _, err := env.tm.Get(context.Background(), env.h, c.uid)
			var ref *seederRefusal
			if !errors.As(err, &ref) || ref.status != c.status || ref.reason != c.reason {
				t.Fatalf("err = %v, want %d %s", err, c.status, c.reason)
			}
			if n := len(env.defaultClient(t).Torrents()) + env.accountTorrents(); n != 0 {
				t.Fatalf("%d torrents loaded after a refusal, want 0", n)
			}
		})
	}
}

// Enforce is the only behaviour: neither the switch's gauge nor the observe
// fallback's counter is exported.
func TestPrivate_NoModeMetrics(t *testing.T) {
	n, err := testutil.GatherAndCount(prometheus.DefaultGatherer,
		"torrent_web_seeder_private_enforce",
		"torrent_web_seeder_private_store_tracker_fallback_total")
	if err != nil || n != 0 {
		t.Fatalf("(%d, %v), want no series", n, err)
	}
}

// A pair already loaded is served without asking the store: neither its outage nor
// the end of the 60 s cache cuts a running stream.
func TestGet_LoadedPairSurvivesStoreOutage(t *testing.T) {
	env := newPrivateEnv(t)
	env.setRecords(testRecord{uidA, keyA})
	first, _, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil {
		t.Fatal(err)
	}
	env.store.setRecord(func(*ts.PullRequest) (*ts.PullReply, error) {
		return nil, status.Error(codes.Unavailable, "down")
	})
	env.tm.records.Drop(entryKey(env.h, uidA))
	asked := env.store.lookupCount()
	again, _, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil || again != first || env.store.lookupCount() != asked {
		t.Fatalf("(%v, %v), lookups +%d: want the loaded torrent, no lookup", again == first, err, env.store.lookupCount()-asked)
	}
}

// A slow record lookup holds up only the request waiting for it: a public
// torrent is served meanwhile.
func TestGet_SlowRecordLookupDoesNotBlockPublic(t *testing.T) {
	env := newPrivateEnv(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	env.store.setRecord(func(*ts.PullRequest) (*ts.PullReply, error) {
		<-release
		return nil, storeErr(codes.PermissionDenied, "no_record")
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

// A player's first Range requests of one pair, all at once, add the torrent
// once: one torrent, one account Client, active_torrents_count +1.
func TestGet_ConcurrentFirstRequestsTrackOnce(t *testing.T) {
	env := newPrivateEnv(t)
	env.setRecords(testRecord{uidA, keyA})
	// A slow lookup: every request is past the first check when the answer
	// comes, so all of them race for the second.
	found := env.store.record
	env.store.setRecord(func(in *ts.PullRequest) (*ts.PullReply, error) {
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
