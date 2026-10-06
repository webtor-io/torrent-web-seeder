package services

import (
	"regexp"

	log "github.com/sirupsen/logrus"
)

// thp passes a client's query on to the seeder whole: the API key and the
// token ride in it, and in the Referer of a page that carries them. The
// access log kept both fields as they came (Loki, 2026-10-05/06: 998k lines
// a day with a key). No other line the seeder writes had either that day.

// credentialParam is a credential query parameter and its value. thp reads
// them under these names only (r.URL.Query().Get), case and all.
var credentialParam = regexp.MustCompile(`([?&](?:api-key|token)=)[^&]+`)

// redactURL is s with the values of its credential parameters replaced by
// "<redacted>"; its path and other parameters as they are.
func redactURL(s string) string {
	return credentialParam.ReplaceAllString(s, "${1}<redacted>")
}

// redactHook runs every string field of a line through redactURL before the
// line is written.
type redactHook struct{}

func (redactHook) Levels() []log.Level { return log.AllLevels }

func (redactHook) Fire(e *log.Entry) error {
	for k, v := range e.Data {
		if s, ok := v.(string); ok {
			e.Data[k] = redactURL(s)
		}
	}
	return nil
}
