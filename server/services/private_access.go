package services

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfave/cli"
)

const (
	PrivateAccessEnforceFlag = "private-access-enforce"
)

// RegisterPrivateFlags registers the switch between observing and enforcing
// access to private torrents. It goes away with the fallback.
func RegisterPrivateFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.BoolFlag{
			Name: PrivateAccessEnforceFlag,
			Usage: "refuse a private torrent to a viewer without a record of their own; " +
				"off: load it with the stored trackers and count the refusal",
			EnvVar: "PRIVATE_ACCESS_ENFORCE",
		},
	)
}

// Reasons of a refusal (X-Seeder-Reason). The first four are what enforce
// answers and observe falls back from.
const (
	reasonPrivateDisabled  = "private_disabled"
	reasonAccountRequired  = "account_required"
	reasonNotEntitled      = "not_entitled"
	reasonBadRecord        = "bad_record"
	reasonBusy             = "busy"
	reasonStoreUnavailable = "store_unavailable"
)

// seederRefusal is why a private torrent is not served: 403, or 503 for a
// condition that passes.
type seederRefusal struct {
	status int    // 403 or 503
	reason string // X-Seeder-Reason
}

func (e *seederRefusal) Error() string { return "seeder refused: " + e.reason }

// writeRefusal answers a seederRefusal: its status, X-Seeder-Reason, and Retry-After: 5
// on a 503; the body is the reason. It reports false, writing nothing, for any other error.
func writeRefusal(w http.ResponseWriter, err error) bool {
	var r *seederRefusal
	if !errors.As(err, &r) {
		return false
	}
	w.Header().Set("X-Seeder-Reason", r.reason)
	if r.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	http.Error(w, r.reason, r.status)
	return true
}

var uidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validUID(uid string) bool { return uidRe.MatchString(uid) }

// metainfoPrivate reports whether mi's info is private (BEP 27).
func metainfoPrivate(mi *metainfo.MetaInfo) (bool, error) {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return false, err
	}
	return info.Private != nil && *info.Private, nil
}

var (
	promPrivateFallback = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torrent_web_seeder_private_store_tracker_fallback_total",
		Help: "Requests for a private torrent served with the stored trackers in observe mode, by the refusal enforce would have answered.",
	}, []string{"reason"})
	promPrivateEnforce = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_private_enforce",
		Help: "1 when access to private torrents is enforced, 0 when observed.",
	})
)

func init() {
	prometheus.MustRegister(promPrivateFallback, promPrivateEnforce)
	for _, r := range []string{reasonAccountRequired, reasonNotEntitled, reasonPrivateDisabled, reasonBadRecord} {
		promPrivateFallback.WithLabelValues(r)
	}
}

// fallback answers what enforce refuses for reason: 403 under enforce; under
// observe it counts reason and loads h as before private records, into the
// default Client with the stored trackers.
func (s *TorrentMap) fallback(h string, mi *metainfo.MetaInfo, reason string) (*torrent.Torrent, string, error) {
	if s.enforce {
		return nil, "", &seederRefusal{http.StatusForbidden, reason}
	}
	promPrivateFallback.WithLabelValues(reason).Inc()
	return s.getDefault(h, mi)
}
