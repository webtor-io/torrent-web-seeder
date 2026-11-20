// Package uploads tells IDs whose content a user uploaded to a home node from
// infohashes. It holds the predicates only; the seeder's branch that serves
// such IDs lives in services.
//
// Each predicate is named by the question it answers, not by the deployment it
// runs in. The ID is taken as it comes: the seeder reads X-Info-Hash raw, so an
// ID in upper case is not of the class here, even where a proxy in front of
// the seeder lower-cases it before it asks.
package uploads

import "regexp"

// Prefix starts every ID of the class: 12 hex, then the kind (1 hex), then 27
// hex.
const Prefix = "9c5db5752b7c"

// Strict, not a prefix check: the ID comes from a request header as is, and a
// branch taken on "9c5db5752b7c/../x" would build a path outside its dir.
var homeHostedR = regexp.MustCompile("^" + Prefix + "[0-9a-f]{28}$")

// HomeHosted answers whether id's content lives on its home node rather than
// in a swarm. Any kind.
func HomeHosted(id string) bool {
	return homeHostedR.MatchString(id)
}

// Owned answers whether id has a single owner, who alone writes it and sees
// it: kind 0, a user's upload.
func Owned(id string) bool {
	return HomeHosted(id) && id[12] == '0'
}
