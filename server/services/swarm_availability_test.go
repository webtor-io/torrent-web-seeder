package services

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring"
)

// bools returns n flags with the listed indices set.
func bools(n int, set ...int) []bool {
	b := make([]bool, n)
	for _, i := range set {
		b[i] = true
	}
	return b
}

// peer returns a peer's piece set with the listed torrent indices.
func peer(set ...int) *roaring.Bitmap {
	bm := roaring.New()
	for _, i := range set {
		bm.Add(uint32(i))
	}
	return bm
}

// peerRange returns a peer's piece set holding [begin, end).
func peerRange(begin, end int) *roaring.Bitmap {
	bm := roaring.New()
	bm.AddRange(uint64(begin), uint64(end))
	return bm
}

const settled = availabilitySettle

func TestComputeAvailability(t *testing.T) {
	for _, c := range []struct {
		name string
		in   availabilityInput
		want availability
	}{
		{
			name: "no peers and nothing here: unknown, nothing painted",
			in:   availabilityInput{complete: bools(10), wanted: bools(10, 0, 1)},
			want: availability{fraction: 0, known: false},
		},
		{
			name: "no peers for long: still unknown, the fraction is what is here",
			in:   availabilityInput{complete: bools(10, 0, 1, 2), peersFor: time.Hour},
			want: availability{fraction: 0.3, known: false},
		},
		{
			// A cached file on a dead swarm: nothing can be missing, whatever
			// peers announce later. StatStream's last frame for a finished
			// file must not say "unknown" for good.
			name: "everything here, no peers: known, exactly 1",
			in:   availabilityInput{complete: bools(3, 0, 1, 2)},
			want: availability{fraction: 1, known: true},
		},
		{
			name: "everything here or already announced during warm-up: known",
			in: availabilityInput{
				complete:  bools(4, 0, 1),
				peers:     []*roaring.Bitmap{peer(2, 3)},
				connected: 1,
				peersFor:  time.Second,
			},
			want: availability{fraction: 1, known: true},
		},
		{
			// The glue does not walk the peers when a seeder is connected,
			// so the seeder case arrives without piece sets.
			name: "a connected seeder: everything is available, from the first second",
			in:   availabilityInput{complete: bools(10), wanted: bools(10, 3, 4), reading: bools(10, 3), seeders: 1},
			want: availability{fraction: 1, known: true},
		},
		{
			name: "partial peers covering everything between them",
			in: availabilityInput{
				complete: bools(10), wanted: bools(10, 0, 9),
				peers:     []*roaring.Bitmap{peerRange(0, 5), peerRange(5, 10)},
				connected: 2,
				peersFor:  settled,
			},
			want: availability{fraction: 1, known: true, missing: []pieceRange{}},
		},
		{
			name: "holes: pieces nobody connected has and we do not have",
			in: availabilityInput{
				complete:  bools(10, 5),
				peers:     []*roaring.Bitmap{peer(0, 1, 2)},
				connected: 1,
				peersFor:  settled,
			},
			want: availability{fraction: 0.4, known: true, missing: []pieceRange{{3, 5}, {6, 10}}},
		},
		{
			name: "wanted pieces without a source are counted; wanted but here or on a peer are not",
			in: availabilityInput{
				complete:  bools(10, 5),
				wanted:    bools(10, 2, 3, 4, 5, 7),
				peers:     []*roaring.Bitmap{peer(0, 1, 2)},
				connected: 1,
				peersFor:  settled,
			},
			want: availability{fraction: 0.4, known: true, missing: []pieceRange{{3, 5}, {6, 10}}, wantedMissing: 3},
		},
		{
			// A warm-up's sticky High on 7..8 is wanted; the reader sits on
			// 2..4, of which 2 is on a peer and 3..4 on nobody.
			name: "reader pieces without a source are counted apart from the wanted ones",
			in: availabilityInput{
				complete:  bools(10, 5),
				wanted:    bools(10, 2, 3, 4, 7, 8),
				reading:   bools(10, 2, 3, 4, 5),
				peers:     []*roaring.Bitmap{peer(0, 1, 2)},
				connected: 1,
				peersFor:  settled,
			},
			want: availability{fraction: 0.4, known: true, missing: []pieceRange{{3, 5}, {6, 10}}, wantedMissing: 4, readerMissing: 2},
		},
		{
			name: "warm-up: peers connected under the settle time are not believed yet",
			in: availabilityInput{
				complete:  bools(10, 5),
				wanted:    bools(10, 3),
				reading:   bools(10, 3),
				peers:     []*roaring.Bitmap{peer(0, 1, 2)},
				connected: 1,
				peersFor:  settled - time.Millisecond,
			},
			want: availability{fraction: 0.4, known: false},
		},
		{
			// The case this is for: peers are here, none has anything.
			name: "connected peers with nothing: everything not here is missing",
			in: availabilityInput{
				complete: bools(4, 0), wanted: bools(4, 1, 2),
				peers:     []*roaring.Bitmap{roaring.New(), roaring.New()},
				connected: 2,
				peersFor:  settled,
			},
			want: availability{fraction: 0.25, known: true, missing: []pieceRange{{1, 4}}, wantedMissing: 2},
		},
		{
			name: "settled but nobody connected right now: unknown",
			in:   availabilityInput{complete: bools(4), peersFor: settled},
			want: availability{fraction: 0, known: false},
		},
		{
			name: "a web seed with part of the torrent: which part is not readable, unknown",
			in: availabilityInput{
				complete: bools(4), wanted: bools(4, 3),
				peers:           []*roaring.Bitmap{peer(0)},
				connected:       1,
				partialWebseeds: 1, peersFor: settled,
			},
			want: availability{fraction: 0.25, known: false},
		},
		{
			name: "a partial web seed, but nothing is missing: known",
			in: availabilityInput{
				complete:        bools(4, 0, 1),
				peers:           []*roaring.Bitmap{peer(2, 3)},
				connected:       1,
				partialWebseeds: 1, peersFor: settled,
			},
			want: availability{fraction: 1, known: true},
		},
		{
			// readSwarm counts a web seed serving the whole torrent as a
			// seeder; next to a partial one it decides the same way.
			name: "a whole-torrent source next to a partial web seed: the whole one decides",
			in:   availabilityInput{complete: bools(4), seeders: 1, partialWebseeds: 1},
			want: availability{fraction: 1, known: true},
		},
		{
			// A file whose first piece is torrent piece 100: peer sets are
			// torrent-indexed, the result is relative to the file like
			// fileStat's positions. Pieces outside the file do not count.
			name: "file scope: torrent indices map to the file's own positions",
			in: availabilityInput{
				offset:    100,
				complete:  bools(5),
				wanted:    bools(5, 2, 3),
				reading:   bools(5, 3, 4),
				peers:     []*roaring.Bitmap{peer(3, 99, 100, 101, 104, 105, 200)},
				connected: 1,
				peersFor:  settled,
			},
			want: availability{fraction: 0.6, known: true, missing: []pieceRange{{2, 4}}, wantedMissing: 2, readerMissing: 1},
		},
		{
			name: "empty scope: nothing to fetch, not NaN",
			in:   availabilityInput{complete: nil},
			want: availability{fraction: 1, known: true},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := computeAvailability(c.in)
			if got.fraction != c.want.fraction || got.known != c.want.known || got.wantedMissing != c.want.wantedMissing || got.readerMissing != c.want.readerMissing {
				t.Errorf("fraction=%v known=%v wantedMissing=%d readerMissing=%d, want %v/%v/%d/%d",
					got.fraction, got.known, got.wantedMissing, got.readerMissing, c.want.fraction, c.want.known, c.want.wantedMissing, c.want.readerMissing)
			}
			if len(got.missing) != len(c.want.missing) || (len(got.missing) > 0 && !reflect.DeepEqual(got.missing, c.want.missing)) {
				t.Errorf("missing=%v, want %v", got.missing, c.want.missing)
			}
		})
	}
}

// A peer that sent HAVE_ALL before the torrent's info was known claims
// every index up to 2^32 (Peer.newPeerPieces adds [0, ToEnd)). The union
// must be read only as far as the scope goes.
func TestComputeAvailability_UnboundedPeerSet(t *testing.T) {
	all := roaring.New()
	all.AddRange(0, 1<<32)
	done := make(chan availability, 1)
	go func() {
		done <- computeAvailability(availabilityInput{offset: 3, complete: bools(4), peers: []*roaring.Bitmap{all}, connected: 1, peersFor: settled})
	}()
	select {
	case got := <-done:
		if got.fraction != 1 || !got.known || len(got.missing) != 0 {
			t.Fatalf("got %+v, want everything available", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("walked the whole 2^32 range of an unbounded peer set")
	}
}

// One snapshot of a torrent's peers serves every scope asked about it in
// that second, concurrently (Stat.swarms): computing a scope must not touch
// the peers' sets.
func TestComputeAvailability_LeavesPeerSetsAlone(t *testing.T) {
	peers := []*roaring.Bitmap{peerRange(0, 100), peer(5, 150), peerRange(60, 70)}
	want := make([]*roaring.Bitmap, len(peers))
	for i, p := range peers {
		want[i] = p.Clone()
	}
	var wg sync.WaitGroup
	for _, offset := range []int{0, 50, 120} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			computeAvailability(availabilityInput{offset: offset, complete: bools(40), peers: peers, connected: 3, peersFor: settled})
		}()
	}
	wg.Wait()
	for i := range peers {
		if !peers[i].Equals(want[i]) {
			t.Errorf("peer %d's set changed: %v, was %v", i, peers[i], want[i])
		}
	}
}

func TestCapRanges(t *testing.T) {
	rs := []pieceRange{{0, 1}, {3, 4}, {5, 6}, {10, 11}, {12, 13}}
	// gaps: 2, 1, 4, 1
	for _, c := range []struct {
		max  int
		want []pieceRange
	}{
		{5, rs},
		{4, []pieceRange{{0, 1}, {3, 6}, {10, 11}, {12, 13}}}, // the first of the two 1-gaps
		{3, []pieceRange{{0, 1}, {3, 6}, {10, 13}}},
		{2, []pieceRange{{0, 6}, {10, 13}}},
		{1, []pieceRange{{0, 13}}},
	} {
		in := append([]pieceRange(nil), rs...)
		if got := capRanges(in, c.max); !reflect.DeepEqual(got, c.want) {
			t.Errorf("capRanges(max=%d) = %v, want %v", c.max, got, c.want)
		}
	}
}

// Through the whole computation: a torrent with more holes than the cap
// comes back with exactly the cap, every missing piece still inside a
// range, and the ends of the torrent's holes where they were.
func TestComputeAvailability_CapsRanges(t *testing.T) {
	const n = 4 * maxMissingRanges
	complete := make([]bool, n)
	// Every other piece is here: 2*maxMissingRanges one-piece holes.
	for i := 0; i < n; i += 2 {
		complete[i] = true
	}
	got := computeAvailability(availabilityInput{complete: complete, peers: []*roaring.Bitmap{roaring.New()}, connected: 1, peersFor: settled})
	if len(got.missing) != maxMissingRanges {
		t.Fatalf("%d ranges, want the cap %d", len(got.missing), maxMissingRanges)
	}
	if got.missing[0].start != 1 || got.missing[len(got.missing)-1].end != n {
		t.Errorf("ranges span [%d, %d), want [1, %d)", got.missing[0].start, got.missing[len(got.missing)-1].end, n)
	}
	for i := 1; i < n; i += 2 {
		in := false
		for _, r := range got.missing {
			in = in || (i >= r.start && i < r.end)
		}
		if !in {
			t.Fatalf("missing piece %d is in no range", i)
		}
	}
	if got.fraction != 0.5 {
		t.Errorf("fraction=%v, want 0.5: the cap coarsens the ranges, not the number", got.fraction)
	}
}

// BenchmarkComputeAvailability is the pure part of a stat for a torrent
// without a connected seeder. Peers hold random halves of the torrent: dense
// bitmap containers without runs, the most expensive shape to clone and to
// union.
//
//   - union: what readSwarm does besides the locks and clones, folding each
//     peer's set into one — once per torrent per second.
//   - scope=torrent, scope=file200: computeAvailability from that union for
//     the whole torrent and for a 200-piece file in the middle — once per
//     path per refresh.
//   - scope=file200/per-peer-sets: the same file from one set per peer, the
//     shape before the union was built once: O(peers × scope).
func BenchmarkComputeAvailability(b *testing.B) {
	for _, pieces := range []int{8 << 10, 64 << 10} {
		for _, peers := range []int{50, 80, 300} {
			b.Run(fmt.Sprintf("pieces=%d/peers=%d", pieces, peers), func(b *testing.B) {
				in := benchAvailabilityInput(pieces, peers, 0.5)
				b.Run("union", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						benchUnion(in.peers)
					}
				})
				perPeer := in.peers
				in.peers = []*roaring.Bitmap{benchUnion(perPeer)}
				b.Run("scope=torrent", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						computeAvailability(in)
					}
				})
				file := fileScope(in, pieces/2, 200)
				b.Run("scope=file200", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						computeAvailability(file)
					}
				})
				file.peers = perPeer
				b.Run("scope=file200/per-peer-sets", func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						computeAvailability(file)
					}
				})
			})
		}
	}
	// The cap's worst case: every other piece missing.
	b.Run("pieces=65536/alternating-holes", func(b *testing.B) {
		complete := make([]bool, 64<<10)
		for i := 0; i < len(complete); i += 2 {
			complete[i] = true
		}
		in := availabilityInput{complete: complete, wanted: make([]bool, len(complete)), peers: []*roaring.Bitmap{roaring.New()}, connected: 1, peersFor: settled}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			computeAvailability(in)
		}
	})
}

// benchUnion folds sets into one the way readSwarm folds the peers' clones.
func benchUnion(sets []*roaring.Bitmap) *roaring.Bitmap {
	u := roaring.New()
	for _, s := range sets {
		u.Or(s)
	}
	return u
}

// fileScope is in narrowed to the n pieces from torrent piece offset.
func fileScope(in availabilityInput, offset, n int) availabilityInput {
	in.offset = offset
	in.complete = in.complete[offset : offset+n]
	in.wanted = in.wanted[offset : offset+n]
	return in
}

// BenchmarkPeerPiecesClone is what PeerConn.PeerPieces costs besides the
// lock: a Clone of the peer's roaring bitmap (Peer.newPeerPieces), once per
// connected peer per stat.
func BenchmarkPeerPiecesClone(b *testing.B) {
	for _, pieces := range []int{8 << 10, 64 << 10} {
		for _, peers := range []int{50, 300} {
			b.Run(fmt.Sprintf("pieces=%d/peers=%d", pieces, peers), func(b *testing.B) {
				in := benchAvailabilityInput(pieces, peers, 0.5)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for _, p := range in.peers {
						_ = p.Clone()
					}
				}
			})
		}
	}
}

func benchAvailabilityInput(pieces, peers int, have float64) availabilityInput {
	r := rand.New(rand.NewPCG(1, 2))
	in := availabilityInput{
		complete: make([]bool, pieces),
		wanted:   make([]bool, pieces),
		peersFor: settled,
	}
	for i := range in.complete {
		in.complete[i] = r.Float64() < 0.1
		in.wanted[i] = !in.complete[i] && r.Float64() < 0.2
	}
	// Every fourth piece is on no peer: a quarter of the torrent in
	// one-piece holes, far more ranges than the cap, so the benchmark pays
	// for the merge too. Real holes are mostly long runs and cheaper.
	for p := 0; p < peers; p++ {
		bm := roaring.New()
		for i := 0; i < pieces; i++ {
			if i%4 != 3 && r.Float64() < have {
				bm.Add(uint32(i))
			}
		}
		in.peers = append(in.peers, bm)
	}
	in.connected = peers
	return in
}
