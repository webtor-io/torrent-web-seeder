package services

import (
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

func TestParseSingleRange(t *testing.T) {
	const fileLen = int64(1000)
	tests := []struct {
		name      string
		header    string
		fileLen   int64
		wantStart int64
		wantEnd   int64
		wantErr   bool
	}{
		{name: "empty header → whole file", header: "", fileLen: fileLen, wantStart: 0, wantEnd: 999},
		{name: "closed range", header: "bytes=0-99", fileLen: fileLen, wantStart: 0, wantEnd: 99},
		{name: "single byte", header: "bytes=0-0", fileLen: fileLen, wantStart: 0, wantEnd: 0},
		{name: "middle range", header: "bytes=200-499", fileLen: fileLen, wantStart: 200, wantEnd: 499},
		{name: "open-ended start- → file end", header: "bytes=500-", fileLen: fileLen, wantStart: 500, wantEnd: 999},
		{name: "suffix range -N", header: "bytes=-100", fileLen: fileLen, wantStart: 900, wantEnd: 999},
		{name: "suffix range larger than file is clamped", header: "bytes=-5000", fileLen: fileLen, wantStart: 0, wantEnd: 999},
		{name: "end past EOF is clamped", header: "bytes=0-99999", fileLen: fileLen, wantStart: 0, wantEnd: 999},
		{name: "multi-range honours first only", header: "bytes=0-99,200-299", fileLen: fileLen, wantStart: 0, wantEnd: 99},
		{name: "whitespace tolerated", header: "bytes= 100 - 199 ", fileLen: fileLen, wantStart: 100, wantEnd: 199},

		{name: "empty file errors", header: "", fileLen: 0, wantErr: true},
		{name: "wrong unit errors", header: "kb=0-99", fileLen: fileLen, wantErr: true},
		{name: "no dash errors", header: "bytes=100", fileLen: fileLen, wantErr: true},
		{name: "both empty errors", header: "bytes=-", fileLen: fileLen, wantErr: true},
		{name: "non-numeric errors", header: "bytes=abc-def", fileLen: fileLen, wantErr: true},
		{name: "end before start errors", header: "bytes=500-100", fileLen: fileLen, wantErr: true},
		{name: "start past EOF errors", header: "bytes=1000-", fileLen: fileLen, wantErr: true},
		{name: "negative start errors", header: "bytes=-0", fileLen: fileLen, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := parseSingleRange(tt.header, tt.fileLen)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got start=%d end=%d", start, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("got [%d,%d], want [%d,%d]", start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

// fpState builds a torrent.FilePieceState for tests. Bytes is the number
// of file bytes covered by this piece (matches FilePieceState semantics:
// boundary pieces only count the slice belonging to the file).
func fpState(bytes int64, complete bool) torrent.FilePieceState {
	return torrent.FilePieceState{
		Bytes: bytes,
		PieceState: torrent.PieceState{
			Completion: storage.Completion{Ok: true, Complete: complete},
		},
	}
}

func TestWarmupBytes(t *testing.T) {
	// Reference layout: 4 pieces × 100 bytes = 400-byte file. Some tests
	// override this with a custom slice when they need an odd last piece
	// or an offset first piece.
	uniform := func(complete ...bool) []torrent.FilePieceState {
		out := make([]torrent.FilePieceState, len(complete))
		for i, c := range complete {
			out[i] = fpState(100, c)
		}
		return out
	}

	tests := []struct {
		name       string
		state      []torrent.FilePieceState
		rangeStart int64
		rangeEnd   int64
		want       int64
	}{
		{
			name:       "empty state → 0",
			state:      nil,
			rangeStart: 0, rangeEnd: 99,
			want: 0,
		},
		{
			name:       "single piece, complete, exact range",
			state:      uniform(true),
			rangeStart: 0, rangeEnd: 99,
			want: 100,
		},
		{
			name:       "single piece, incomplete",
			state:      uniform(false),
			rangeStart: 0, rangeEnd: 99,
			want: 0,
		},
		{
			name:       "whole file, all complete",
			state:      uniform(true, true, true, true),
			rangeStart: 0, rangeEnd: 399,
			want: 400,
		},
		{
			name:       "whole file, half complete (first half)",
			state:      uniform(true, true, false, false),
			rangeStart: 0, rangeEnd: 399,
			want: 200,
		},
		{
			name:       "range crossing 3 pieces, middle missing",
			state:      uniform(true, false, true, true),
			rangeStart: 50, rangeEnd: 249,
			// piece 0: bytes 50..99 (50), piece 1: skipped, piece 2: bytes 200..249 (50)
			want: 100,
		},
		{
			name:       "range crossing 3 pieces, all complete",
			state:      uniform(true, true, true, true),
			rangeStart: 50, rangeEnd: 249,
			// piece 0: 50, piece 1: 100, piece 2: 50
			want: 200,
		},
		{
			name:       "range fully inside one piece",
			state:      uniform(true, true, true, true),
			rangeStart: 130, rangeEnd: 170,
			want: 41,
		},
		{
			name:       "range fully inside one incomplete piece",
			state:      uniform(true, false, true, true),
			rangeStart: 130, rangeEnd: 170,
			want: 0,
		},
		{
			name:       "ignores pieces outside range",
			state:      uniform(true, true, true, true),
			rangeStart: 100, rangeEnd: 199,
			want: 100,
		},
		{
			name: "tail-style file with short final piece",
			// File is 350 bytes: 3 full pieces (100 each) + 1 partial (50).
			state:      []torrent.FilePieceState{fpState(100, true), fpState(100, true), fpState(100, true), fpState(50, true)},
			rangeStart: 0, rangeEnd: 349,
			want: 350,
		},
		{
			name:       "tail piece only, complete",
			state:      []torrent.FilePieceState{fpState(100, false), fpState(100, false), fpState(100, false), fpState(50, true)},
			rangeStart: 300, rangeEnd: 349,
			want: 50,
		},
		{
			name: "file straddles a piece boundary (head piece is partial)",
			// Mimics File.State() when the file starts mid-piece: first
			// FilePieceState reports only the file-bytes inside that piece.
			state:      []torrent.FilePieceState{fpState(60, true), fpState(100, true), fpState(40, true)},
			rangeStart: 0, rangeEnd: 199,
			want: 200,
		},
		{
			name:       "range collapses to single byte on a piece boundary",
			state:      uniform(true, true, true, true),
			rangeStart: 100, rangeEnd: 100,
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := warmupBytes(tt.state, tt.rangeStart, tt.rangeEnd)
			if got != tt.want {
				t.Fatalf("warmupBytes = %d, want %d", got, tt.want)
			}
		})
	}
}
