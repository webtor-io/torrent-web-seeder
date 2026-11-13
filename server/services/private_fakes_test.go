package services

import (
	"context"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Accounts and passkeys of the private-torrent tests. The keys are made up
// and every tracker is tracker.invalid or a loopback fake.
const (
	uidA      = "0123456789abcdef0123456789abcdef01234567" // acct deb87fab
	uidB      = "89abcdef0123456789abcdef0123456789abcdef" // acct 5f44da3a
	keyStored = "00000000000000000000000000000000"         // first uploader's key in the store's copy
	keyA      = "000000000000000000000000000000aa"
	keyA2     = "000000000000000000000000000000a2"
	keyB      = "000000000000000000000000000000bb"
)

// storeFake is torrent-store's client: Pull answered from torrents (NotFound
// otherwise), GetPrivateRecord by record (nil: Unimplemented); every request kept.
type storeFake struct {
	ts.TorrentStoreClient
	mu       sync.Mutex
	torrents map[string][]byte
	record   func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error)
	pulls    []*ts.PullRequest
	lookups  int
}

func (f *storeFake) store() *TorrentStore {
	return &TorrentStore{cl: f, inited: true}
}

func (f *storeFake) Pull(_ context.Context, in *ts.PullRequest, _ ...grpc.CallOption) (*ts.PullReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, in)
	b, ok := f.torrents[in.GetInfoHash()]
	if !ok {
		return nil, status.Error(codes.NotFound, "unable to find torrent")
	}
	return &ts.PullReply{Torrent: b}, nil
}

// GetPrivateRecord counts the lookup before answering, so a test can see one
// that record holds.
func (f *storeFake) GetPrivateRecord(_ context.Context, in *ts.GetPrivateRecordRequest, _ ...grpc.CallOption) (*ts.GetPrivateRecordReply, error) {
	f.mu.Lock()
	f.lookups++
	record := f.record
	f.mu.Unlock()
	if record == nil {
		return nil, status.Error(codes.Unimplemented, "unknown method GetPrivateRecord")
	}
	return record(in)
}

func (f *storeFake) setRecord(fn func(*ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = fn
}

func (f *storeFake) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func (f *storeFake) pullRequests() []*ts.PullRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*ts.PullRequest(nil), f.pulls...)
}

// privatePayload is addTestPayload with info.private = 1 and one tier of
// announce; announce also names the first, as private .torrent files do.
func privatePayload(t *testing.T, announce ...string) ([]byte, *metainfo.MetaInfo) {
	t.Helper()
	data, mi := addTestPayload(t)
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	private := true
	info.Private = &private
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}
	if len(announce) > 0 {
		mi.Announce = announce[0]
		mi.AnnounceList = metainfo.AnnounceList{announce}
	}
	return data, mi
}

func torrentBytes(t *testing.T, mi *metainfo.MetaInfo) []byte {
	t.Helper()
	b, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
