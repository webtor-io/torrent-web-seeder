package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// No log line and no metric carries a uid or a tracker key, along every private
// path: load, refusals, a tracker that refuses quoting the key, revocation,
// shutdown, at every level. The lines of A's refused announce and C's failed
// lookup are seen, so the check is not vacuous.
func TestPrivate_NoUidOrKeyInLogsOrMetrics(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	prevLevel := log.GetLevel()
	log.SetLevel(log.TraceLevel)
	defer log.SetLevel(prevLevel)
	setVar(t, &recordRefreshInterval, 100*time.Millisecond)
	env := newPrivateEnv(t, true)
	env.trk.Refuse(keyA)
	env.setRecords(testRecord{uidA, keyA})
	torA, _, _ := env.tm.Get(context.Background(), env.h, uidA)
	waitFor(t, "A's tracker to refuse", torA.AllAnnouncesFailing)
	var ref *seederRefusal
	if _, _, err := env.tm.Get(context.Background(), env.h, uidB); !errors.As(err, &ref) || ref.reason != reasonNotEntitled {
		t.Fatalf("B: %v, want not_entitled", err)
	}
	env.store.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return nil, status.Error(codes.Unavailable, "s3 "+keyA)
	})
	const uidC = "fedcba9876543210fedcba9876543210fedcba98" // acct bafec2d5; not in the cache: B's missing is
	// store_unavailable; the error text echoes a key
	if _, _, err := env.tm.Get(context.Background(), env.h, uidC); !errors.As(err, &ref) || ref.reason != reasonStoreUnavailable {
		t.Fatalf("C: %v, want store_unavailable", err)
	}
	env.setRecords()
	waitFor(t, "the revoked pair to unload", func() bool { // a refused announcer may send no stopped (#9)
		env.tm.mux.Lock()
		defer env.tm.mux.Unlock()
		return env.tm.entries[entryKey(env.h, uidA)] == nil
	})
	env.tm.CloseAccounts(time.Second)
	var sawAcct, sawAcctC, sawEcho bool
	for _, e := range hook.AllEntries() {
		line, _ := e.String()
		for _, secret := range []string{uidA, uidB, uidC, keyA, keyStored} {
			if strings.Contains(line, secret) {
				t.Errorf("log line carries %q: %s", secret, line)
			}
		}
		sawAcct = sawAcct || e.Data["acct"] == "deb87fab"
		sawAcctC = sawAcctC || e.Data["acct"] == "bafec2d5"
		sawEcho = sawEcho || e.Message == "announce failed" && strings.Contains(line, "invalid passkey")
	}
	if !sawAcct || !sawAcctC || !sawEcho {
		t.Fatalf("lines seen: acct=deb87fab %v, acct=bafec2d5 %v, A's refused announce %v; the check above is vacuous", sawAcct, sawAcctC, sawEcho)
	}
	mfs, _ := prometheus.DefaultGatherer.Gather()
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				for _, secret := range []string{uidA, uidB, uidC, keyA, keyStored} {
					if strings.Contains(l.GetValue(), secret) {
						t.Errorf("%s{%s=%q}", mf.GetName(), l.GetName(), l.GetValue())
					}
				}
			}
		}
	}
}
