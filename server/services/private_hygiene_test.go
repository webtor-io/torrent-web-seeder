package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus/hooks/test"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// No log line and no metric carries a uid or a tracker key, along every private
// path: load, refusals, a tracker that refuses, revocation, shutdown. At least one
// line carries acct, so the check is not vacuous.
func TestPrivate_NoUidOrKeyInLogsOrMetrics(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	setVar(t, &recordRefreshInterval, 100*time.Millisecond)
	env := newPrivateEnv(t, true)
	env.trk.Refuse(keyA)
	env.setRecords(testRecord{uidA, keyA})
	torA, _, _ := env.tm.Get(context.Background(), env.h, uidA)
	waitFor(t, "A's tracker to refuse", torA.AllAnnouncesFailing)
	env.tm.Get(context.Background(), env.h, uidB) // not_entitled
	env.store.setRecord(func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		return nil, status.Error(codes.Unavailable, "s3 "+keyA)
	})
	const uidC = "fedcba9876543210fedcba9876543210fedcba98" // not in the cache: B's missing is
	env.tm.Get(context.Background(), env.h, uidC)           // store_unavailable; the error text echoes a key
	env.setRecords()
	waitFor(t, "the revoked pair to unload", func() bool { // a refused announcer may send no stopped (#9)
		env.tm.mux.Lock()
		defer env.tm.mux.Unlock()
		return env.tm.entries[entryKey(env.h, uidA)] == nil
	})
	env.tm.CloseAccounts(time.Second)
	var sawAcct bool
	for _, e := range hook.AllEntries() {
		line, _ := e.String()
		for _, secret := range []string{uidA, uidB, uidC, keyA, keyStored} {
			if strings.Contains(line, secret) {
				t.Errorf("log line carries %q: %s", secret, line)
			}
		}
		sawAcct = sawAcct || e.Data["acct"] == "deb87fab"
	}
	if !sawAcct {
		t.Fatal("no line carries acct=deb87fab; the check above is vacuous")
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
