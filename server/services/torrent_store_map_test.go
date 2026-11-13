package services

import "testing"

// The seeder's Pull cache is shared by every viewer of a hash and thp has
// decided access to it already, so the seeder asks as an internal caller
// (decision 34); a store under PRIVATE_ACCESS_ENFORCE refuses one that says nothing.
func TestTorrentStoreMap_PullsAsInternal(t *testing.T) {
	_, mi := privatePayload(t, "http://tracker.invalid/announce?passkey="+keyStored)
	h := mi.HashInfoBytes().HexString()
	f := &storeFake{torrents: map[string][]byte{h: torrentBytes(t, mi)}}
	if _, err := NewTorrentStoreMap(f.store()).Get(h); err != nil {
		t.Fatal(err)
	}
	reqs := f.pullRequests()
	if len(reqs) != 1 {
		t.Fatalf("pulls = %d, want 1", len(reqs))
	}
	if c := reqs[0].GetCaller(); c == nil || !c.GetInternal() || c.GetUid() != "" {
		t.Fatalf("Pull caller = %v, want {internal: true}", c)
	}
}
