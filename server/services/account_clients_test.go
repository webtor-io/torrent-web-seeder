package services

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAccounts_LimitAnswersBusy(t *testing.T) {
	env := newPrivateEnv(t, true)
	setVar(t, &maxAccountClients, 1) // restores on cleanup
	env.setRecords(testRecord{uidA, keyA}, testRecord{uidB, keyB})
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err != nil {
		t.Fatal(err)
	}
	refused := testutil.ToFloat64(promAccountClientsRefused)
	_, _, err := env.tm.Get(context.Background(), env.h, uidB)
	var ref *seederRefusal
	if !errors.As(err, &ref) || ref.status != 503 || ref.reason != reasonBusy {
		t.Fatalf("err = %v, want 503 busy", err)
	}
	if d := testutil.ToFloat64(promAccountClientsRefused) - refused; d != 1 {
		t.Fatalf("refused +%v, want +1", d)
	}
}

func TestAccounts_IdleClientClosesAfterLastTorrent(t *testing.T) {
	setVar(t, &accountIdleTimeout, 200*time.Millisecond) // restores on cleanup
	env := newPrivateEnv(t, true)
	env.tm.ttl = 100 * time.Millisecond
	env.setRecords(testRecord{uidA, keyA})
	gauge := testutil.ToFloat64(promAccountClients)
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err != nil {
		t.Fatal(err)
	}
	// Under the lock: the idle timer deletes from the map.
	env.tm.mux.Lock()
	old := env.tm.accounts[uidA]
	env.tm.mux.Unlock()
	cl := old.cl
	waitFor(t, "the idle account Client to close", func() bool {
		select {
		case <-cl.Closed():
			return true
		default:
			return false
		}
	})
	if len(env.tm.accounts) != 0 || testutil.ToFloat64(promAccountClients) != gauge {
		t.Fatalf("accounts %d, gauge %v, want 0 and %v", len(env.tm.accounts), testutil.ToFloat64(promAccountClients), gauge)
	}
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err != nil {
		t.Fatalf("a request after the close: %v", err)
	}
	// A late timer of the closed Client leaves the new one alone.
	env.tm.closeIdleAccount(uidA, old)
	env.tm.mux.Lock()
	n := len(env.tm.accounts)
	env.tm.mux.Unlock()
	if n != 1 {
		t.Fatalf("accounts %d after the old Client's timer, want 1", n)
	}
}

// Review Focus 3.
func TestAccounts_IdleCloseRacesGet(t *testing.T) {
	setVar(t, &accountIdleTimeout, time.Millisecond)
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	for i := 0; i < 50; i++ {
		tor, key, err := env.tm.Get(context.Background(), env.h, uidA)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		// The losing order, every round: an idle timer that fired before
		// this Get and waited for the lock runs after it.
		env.tm.mux.Lock()
		a := env.tm.accounts[uidA]
		env.tm.mux.Unlock()
		env.tm.closeIdleAccount(uidA, a)
		select {
		case <-tor.Closed():
			t.Fatalf("round %d: served a torrent of a closed Client", i)
		default:
		}
		env.tm.mux.Lock()
		env.tm.unload(key, env.tm.entries[key])
		env.tm.mux.Unlock()
	}
}

// A torrent added to an idle account disarms its idle close, so the Client
// lives accountIdleTimeout past its next last torrent, not past the old one.
func TestAccounts_GetStopsIdleClose(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	_, key, err := env.tm.Get(context.Background(), env.h, uidA)
	if err != nil {
		t.Fatal(err)
	}
	env.tm.mux.Lock()
	env.tm.unload(key, env.tm.entries[key])
	idle := env.tm.accounts[uidA].idle
	env.tm.mux.Unlock()
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err != nil {
		t.Fatal(err)
	}
	if idle.Stop() {
		t.Fatal("the idle close is still armed with a torrent in the Client")
	}
}

func TestAccounts_BuildErrorIsNotSticky(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	var fail atomic.Bool
	fail.Store(true)
	cfg := env.tm.tc.testConfig
	env.tm.tc.testConfig = func(c *torrent.ClientConfig) {
		cfg(c)
		if fail.Load() {
			// RFC 5737, on no interface: the bind fails, with no DNS lookup.
			c.ListenHost = func(string) string { return "192.0.2.1" }
		}
	}
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err == nil {
		t.Fatal("the first build did not fail")
	}
	fail.Store(false)
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err != nil {
		t.Fatalf("the error stuck: %v", err)
	}
}

// refusingStorage fails every OpenTorrent, so AddTorrent fails.
type refusingStorage struct{}

func (refusingStorage) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	return storage.TorrentImpl{}, errors.New("storage refused")
}

// A Client built for a torrent that then fails to add holds nothing: it
// closes when idle like one whose last torrent went.
func TestAccounts_FailedAddClosesIdleClient(t *testing.T) {
	setVar(t, &accountIdleTimeout, 50*time.Millisecond)
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	cfg := env.tm.tc.testConfig
	env.tm.tc.testConfig = func(c *torrent.ClientConfig) {
		cfg(c)
		c.DefaultStorage = refusingStorage{}
	}
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err == nil {
		t.Fatal("the add did not fail")
	}
	waitFor(t, "the account Client to close", func() bool {
		env.tm.mux.Lock()
		defer env.tm.mux.Unlock()
		return len(env.tm.accounts) == 0
	})
}

func TestCloseAccounts_StoppedBeforeClose(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA})
	// A real tracker's name is looked up before every announce, stopped
	// included, and a Client closed meanwhile sends nothing ("client is
	// closed"). The loopback fake needs no lookup, so it gets a slow one.
	cfg := env.tm.tc.testConfig
	env.tm.tc.testConfig = func(c *torrent.ClientConfig) {
		cfg(c)
		c.LookupTrackerIp = func(u *url.URL) ([]net.IP, error) {
			time.Sleep(200 * time.Millisecond)
			return []net.IP{net.ParseIP(u.Hostname())}, nil
		}
	}
	if _, _, err := env.tm.Get(context.Background(), env.h, uidA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.tm.Get(context.Background(), env.pub, ""); err != nil {
		t.Fatal(err)
	}
	env.trk.waitEvent(t, keyA, "started")
	cl := env.tm.accounts[uidA].cl
	env.tm.CloseAccounts(2 * time.Second)
	var stopped bool
	for _, a := range env.trk.Announces(keyA) {
		stopped = stopped || a.event == "stopped"
	}
	if !stopped {
		t.Fatal("no stopped with the account's key before CloseAccounts returned")
	}
	select {
	case <-cl.Closed():
	default:
		t.Fatal("the account Client is still open")
	}
	if len(env.defaultClient(t).Torrents()) != 1 {
		t.Fatal("the default Client lost its torrent")
	}
}

// CloseAccounts leaves no idle close armed on the Clients it closes: neither
// the one its unload arms (A) nor one armed before (B, whose last torrent
// went earlier).
func TestCloseAccounts_LeavesNoIdleTimer(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.setRecords(testRecord{uidA, keyA}, testRecord{uidB, keyB})
	for _, uid := range []string{uidA, uidB} {
		if _, _, err := env.tm.Get(context.Background(), env.h, uid); err != nil {
			t.Fatal(err)
		}
	}
	env.tm.mux.Lock()
	kb := entryKey(env.h, uidB)
	env.tm.unload(kb, env.tm.entries[kb])
	accounts := maps.Clone(env.tm.accounts)
	env.tm.mux.Unlock()
	if accounts[uidB].idle == nil {
		t.Fatal("B's idle close is not armed before CloseAccounts")
	}
	env.tm.CloseAccounts(0)
	for uid, a := range accounts {
		if a.idle != nil && a.idle.Stop() {
			t.Errorf("acct %s: an idle close is armed on a closed Client", acct(uid))
		}
	}
}

// Review Focus 4.
func TestCloseAccounts_NoAccountsReturnsAtOnce(t *testing.T) {
	env := newPrivateEnv(t, true)
	start := time.Now()
	env.tm.CloseAccounts(8 * time.Second)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("took %v with no account Client", d)
	}
}
