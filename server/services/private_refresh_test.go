package services

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/sirupsen/logrus/hooks/test"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unloadReasons lists the reason of every "private torrent unloaded" line, in order.
func unloadReasons(hook *test.Hook) []string {
	var r []string
	for _, e := range hook.AllEntries() {
		if e.Message == "private torrent unloaded" {
			reason, _ := e.Data["reason"].(string)
			r = append(r, reason)
		}
	}
	return r
}

// The 60 s cache is fresh, the record is deleted: the tick reads past the cache.
func TestRefresh_DeletedRecordStopsWithinInterval(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	setVar(t, &recordRefreshInterval, 100*time.Millisecond)
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	_, key, _ := env.tm.Get(context.Background(), env.h, uidA)
	release := env.tm.Hold(key) // being read: revocation cuts it all the same
	defer release()
	env.trk.waitEvent(t, keyA, "started")
	env.setRecords()
	env.trk.waitEvent(t, keyA, "stopped")
	env.tm.mux.Lock()
	_, ok := env.tm.entries[key]
	env.tm.mux.Unlock()
	if got := unloadReasons(hook); ok || !reflect.DeepEqual(got, []string{"gone"}) {
		t.Fatalf("entry kept=%v, unload lines %q; want unloaded and one line with reason=gone", ok, got)
	}
}

// A record whose trackers change while the torrent is read (a new key)
// unloads it, stopped going with the old key, and the next request adds it
// with the new trackers only. The same record on every tick touches nothing.
func TestRefresh_ChangedRecordDropsThenReAdds(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	setVar(t, &recordRefreshInterval, 100*time.Millisecond)
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	first, key, _ := env.tm.Get(context.Background(), env.h, uidA)
	release := env.tm.Hold(key) // being read: a changed record cuts it all the same
	defer release()
	env.trk.waitEvent(t, keyA, "started")
	time.Sleep(3 * recordRefreshInterval) // the same record on every tick
	env.tm.mux.Lock()
	e := env.tm.entries[key]
	env.tm.mux.Unlock()
	if e == nil || e.t != first || len(unloadReasons(hook)) != 0 {
		t.Fatal("an unchanged record unloaded the torrent")
	}
	env.setRecords(testRecord{uidA, keyA2})
	env.trk.waitEvent(t, keyA, "stopped")
	again, _, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil || again == first {
		t.Fatalf("after the change: same torrent=%v, %v; want a new one", again == first, err)
	}
	if mi := again.Metainfo(); !reflect.DeepEqual(mi.UpvertedAnnounceList(), metainfo.AnnounceList{{env.trk.URL(keyA2)}}) {
		t.Fatalf("re-added with %v, want only the new key", mi.UpvertedAnnounceList())
	}
	env.trk.waitEvent(t, keyA2, "started")
	if got := unloadReasons(hook); !reflect.DeepEqual(got, []string{"changed"}) {
		t.Fatalf("unload lines %q, want one with reason=changed", got)
	}
	started := 0
	for _, a := range env.trk.Announces(keyA) {
		if a.event == "started" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("the old key was announced as started %d times, want 1", started)
	}
}

func TestRefresh_UnavailableKeepsTrackers(t *testing.T) {
	setVar(t, &recordRefreshInterval, 100*time.Millisecond)
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	first, key, _ := env.tm.Get(context.Background(), env.h, uidA)
	env.trk.waitEvent(t, keyA, "started")
	env.store.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return nil, status.Error(codes.Unavailable, "down")
	})
	asked := env.store.lookupCount()
	time.Sleep(3 * recordRefreshInterval)
	if env.store.lookupCount() == asked {
		t.Fatal("no tick asked the store; the checks below are vacuous")
	}
	env.tm.mux.Lock()
	e := env.tm.entries[key]
	env.tm.mux.Unlock()
	if e == nil || e.t != first {
		t.Fatal("an unavailable store changed the loaded torrent")
	}
	for _, a := range env.trk.Announces(keyA) {
		if a.event == "stopped" {
			t.Fatal("stopped on an unavailable store")
		}
	}
}

// A loop ends, unloading nothing though the record is gone, when key holds
// another torrent or nothing, and when its torrent closes.
func TestRefresh_StaleLoopEnds(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	loaded, key, _ := env.tm.Get(context.Background(), env.h, uidA) // its own loop ticks every 60 s
	other, _, _ := env.tm.Get(context.Background(), env.pub, "")    // not key's
	env.setRecords()
	ends := func(what, k string, interval time.Duration) {
		t.Helper()
		done := make(chan struct{})
		go func() { env.tm.refreshLoop(k, env.h, uidA, other, interval); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("the loop %s is still running", what)
		}
	}
	ends("under a key holding another torrent", key, 10*time.Millisecond)
	ends("under an empty key", "absent", 10*time.Millisecond)
	other.Drop()
	ends("of a closed torrent", key, time.Hour) // no tick: only the close ends it
	env.tm.mux.Lock()
	e := env.tm.entries[key]
	env.tm.mux.Unlock()
	if e == nil || e.t != loaded {
		t.Fatal("a stale loop unloaded key's torrent")
	}
}
