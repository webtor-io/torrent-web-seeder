package services

import (
	"regexp"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus"
)

// Why the trackers of private torrents refuse our announces, by class, so
// that the share of each can be read without the trackers' text. The text is
// classified and dropped: the library redacts echoes of the announce URL, but
// a tracker may still quote the passkey in a form it does not recognise, so
// the text is neither logged nor a label. The library reports each tracker's
// run of refusals once (Callbacks.PrivateTrackerRefused), so a tracker it asked
// four times weighs as much as one it asked once.
//
// A class says what the viewer can do:
//   - unregistered: the tracker does not know the torrent. Nothing, but another
//     torrent.
//   - key: it does not know the passkey or the user (changed, revoked). A fresh
//     .torrent.
//   - client: it does not let this client, or this server's IP, in. Nothing
//     here: their usual client for that tracker.
//   - account: it knows the user and refuses them (ratio, limits, ban). Sort it
//     out on the tracker.
//   - other: no rule matched. Its share says how far the others can be read.
//
// The classes and rules are magnet2torrent's (server/refusal.go), which
// classifies the refusal of a magnet's key on upload for the API's reason:
// one tracker text gets one class in both. Change them together. The rules
// are tried in order: "torrent not found for this passkey" is about the
// torrent, "banned client" about the client.
var trackerRefusalClasses = []string{"unregistered", "key", "client", "account", "other"}

var trackerRefusalRules = []struct {
	class string
	re    *regexp.Regexp
}{
	{"unregistered", regexp.MustCompile(`(?i)unregistered|not registered|torrent not found|unknown torrent|info_?hash not found|torrent does not exist`)},
	{"key", regexp.MustCompile(`(?i)pass ?key|auth ?key|torrent ?pass|user not found|unknown user|invalid key|invalid user`)},
	{"client", regexp.MustCompile(`(?i)client|white ?list|black ?list|\bip\b`)},
	{"account", regexp.MustCompile(`(?i)account|ratio|limit|disabled|suspended|banned|warned|not allowed to download|leech|slots?`)},
}

var (
	promPrivateTrackerRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torrent_web_seeder_private_tracker_refusals_total",
		Help: "Runs of refusals by a private (BEP 27) torrent's tracker, by what its failure reason says: " +
			"unregistered, key, client, account, other (no rule matched).",
	}, []string{"class"})

	// Each class's counter, built up front so that every class is exported
	// from zero.
	privateTrackerRefusalsByClass = map[string]prometheus.Counter{}
)

func init() {
	prometheus.MustRegister(promPrivateTrackerRefusals)
	for _, l := range trackerRefusalClasses {
		privateTrackerRefusalsByClass[l] = promPrivateTrackerRefusals.WithLabelValues(l)
	}
}

// trackerRefusalClass is the class of the refusal err, by its text.
func trackerRefusalClass(err error) string {
	s := err.Error()
	for _, r := range trackerRefusalRules {
		if r.re.MatchString(s) {
			return r.class
		}
	}
	return "other"
}

// countPrivateTrackerRefusal is the Client's PrivateTrackerRefused callback.
// It runs under the client lock and touches nothing of the torrent.
func countPrivateTrackerRefusal(_ *torrent.Torrent, err error) {
	privateTrackerRefusalsByClass[trackerRefusalClass(err)].Inc()
}
