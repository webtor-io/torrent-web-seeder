package services

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anacrolix/torrent"
	httpTracker "github.com/anacrolix/torrent/tracker/http"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// refusal is an error as the library hands it to PrivateTrackerRefused.
func refusal(reason string) error {
	return fmt.Errorf("%w: %q", httpTracker.ErrFailureReason, reason)
}

func TestTrackerRefusalClass(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		// magnet2torrent's TestClassifyRefusal, which these rules must agree with.
		{"Unregistered torrent", "unregistered"},
		{"Torrent not registered with this tracker", "unregistered"},
		{"Torrent not found for this passkey", "unregistered"},
		{"Invalid passkey", "key"},
		{"passkey not found", "key"},
		{"User not found", "key"},
		{"Invalid passkey for this torrent", "key"},
		{"Your client is not on the whitelist", "client"},
		{"Banned client", "client"},
		{"Your IP is not allowed", "client"},
		{"Your account is disabled", "account"},
		{"Your ratio is too low", "account"},
		{"You have reached your download limit", "account"},
		{"You are not allowed to download (warned)", "account"},
		{"Tracker is down for maintenance", "other"},
		{"", "other"},
		// And as the library reports them here.
		{"Invalid passkey (<redacted>)", "key"},
		{"Your client is not allowed on this account", "client"},
		// "ip" only as a word.
		{"Membership expired, see the shipping page", "other"},
	} {
		if got := trackerRefusalClass(refusal(c.reason)); got != c.want {
			t.Errorf("%q: class %q, want %q", c.reason, got, c.want)
		}
	}
}

// The library's own prefix names no class.
func TestTrackerRefusalClassIgnoresPrefix(t *testing.T) {
	if got := trackerRefusalClass(httpTracker.ErrFailureReason); got != "other" {
		t.Errorf("bare ErrFailureReason: class %q, want other", got)
	}
	if got := trackerRefusalClass(errors.New("passkey")); got != "key" {
		t.Errorf("any error's text is classified: class %q, want key", got)
	}
}

func TestCountPrivateTrackerRefusal(t *testing.T) {
	c := privateTrackerRefusalsByClass["client"]
	before := testutil.ToFloat64(c)
	countPrivateTrackerRefusal(nil, refusal("Your client is not on the whitelist"))
	if d := testutil.ToFloat64(c) - before; d != 1 {
		t.Errorf("client refusals grew by %v, want 1", d)
	}
	for _, l := range trackerRefusalClasses {
		if _, ok := privateTrackerRefusalsByClass[l]; !ok {
			t.Errorf("class %q has no counter", l)
		}
	}
}

// Both the default Client and an account's count their trackers' refusals.
func TestClientsCountPrivateTrackerRefusals(t *testing.T) {
	tc := torrentClientFromFlags(t, "--data-dir", t.TempDir())
	acc := tc.forUser()
	c := privateTrackerRefusalsByClass["key"]
	for name, s := range map[string]*TorrentClient{"default": tc, "account": acc} {
		var cbs []func(*torrent.Torrent, error)
		s.testConfig = func(cfg *torrent.ClientConfig) {
			cbs = cfg.Callbacks.PrivateTrackerRefused
			loopbackConfig(cfg)
		}
		cl, err := s.get()
		if err != nil {
			t.Fatal(err)
		}
		cl.Close()
		before := testutil.ToFloat64(c)
		for _, cb := range cbs {
			cb(nil, refusal("Invalid passkey"))
		}
		if d := testutil.ToFloat64(c) - before; d != 1 {
			t.Errorf("%s Client: a key refusal counted %v times, want 1", name, d)
		}
	}
}
