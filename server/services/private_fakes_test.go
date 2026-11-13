package services

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/anacrolix/torrent"
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
// that record holds. Like a gRPC client, it gives up when ctx ends.
func (f *storeFake) GetPrivateRecord(ctx context.Context, in *ts.GetPrivateRecordRequest, _ ...grpc.CallOption) (*ts.GetPrivateRecordReply, error) {
	f.mu.Lock()
	f.lookups++
	record := f.record
	f.mu.Unlock()
	if record == nil {
		return nil, status.Error(codes.Unimplemented, "unknown method GetPrivateRecord")
	}
	type answer struct {
		r   *ts.GetPrivateRecordReply
		err error
	}
	ch := make(chan answer, 1)
	go func() { r, err := record(in); ch <- answer{r, err} }()
	select {
	case a := <-ch:
		return a.r, a.err
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
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

type trackerAnnounce struct {
	key, event, peerID string
	port               int
}

// fakeTracker answers announces on loopback (trackerAnswers), or refuses a key
// (trackerRefuses); it keeps every announce by its passkey.
type fakeTracker struct {
	srv     *httptest.Server
	mu      sync.Mutex
	seen    map[string][]trackerAnnounce
	refused map[string]bool
}

func newFakeTracker(t *testing.T) *fakeTracker {
	f := &fakeTracker{seen: map[string][]trackerAnnounce{}, refused: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		a := trackerAnnounce{key: q.Get("passkey"), event: q.Get("event"), peerID: q.Get("peer_id")}
		a.port, _ = strconv.Atoi(q.Get("port"))
		f.mu.Lock()
		f.seen[a.key] = append(f.seen[a.key], a)
		body := trackerAnswers
		if f.refused[a.key] {
			body = trackerRefuses
		}
		f.mu.Unlock()
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTracker) URL(key string) string { return f.srv.URL + "/announce?passkey=" + key }

func (f *fakeTracker) Refuse(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused[key] = true
}

func (f *fakeTracker) Announces(key string) []trackerAnnounce {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]trackerAnnounce(nil), f.seen[key]...)
}

// waitEvent waits for an announce of key with event ("started", "stopped").
func (f *fakeTracker) waitEvent(t *testing.T, key, event string) {
	t.Helper()
	waitFor(t, "a "+event+" announce", func() bool {
		for _, a := range f.Announces(key) {
			if a.event == event {
				return true
			}
		}
		return false
	})
}

type testRecord struct{ uid, key string }

// privateEnv: a private torrent (stored copy announcing keyStored) and a public one in
// storeFake; a TorrentMap over a loopback TorrentClient with trackers on
// (trackerTestConfig, allowLocalNetworks, siblings), WithPrivate(records, enforce).
type privateEnv struct {
	tm    *TorrentMap
	store *storeFake
	trk   *fakeTracker
	h     string // the private torrent
	pub   string // the public torrent
	data  []byte
	dir   string
}

func newPrivateEnv(t *testing.T, enforce bool) *privateEnv {
	t.Helper()
	trk := newFakeTracker(t)
	data, mi := privatePayload(t, trk.URL(keyStored))
	_, pub := addTestPayload(t)
	env := &privateEnv{
		store: &storeFake{torrents: map[string][]byte{
			mi.HashInfoBytes().HexString():  torrentBytes(t, mi),
			pub.HashInfoBytes().HexString(): torrentBytes(t, pub),
		}},
		trk:  trk,
		h:    mi.HashInfoBytes().HexString(),
		pub:  pub.HashInfoBytes().HexString(),
		data: data,
		dir:  t.TempDir(),
	}
	tc := &TorrentClient{clientSettings: clientSettings{
		swarm:              newSwarmStats(),
		testConfig:         trackerTestConfig,
		siblings:           newStorageSiblings(),
		maxUnverifiedBytes: -1,
		dataDir:            env.dir,
		allowLocalNetworks: true,
	}}
	env.tm = NewTorrentMap(tc, NewTorrentStoreMap(env.store.store()), &FileStoreMap{}).
		WithPrivate(NewPrivateRecords(env.store.store()), enforce)
	t.Cleanup(func() {
		var cls []*torrent.Client
		env.tm.mux.Lock()
		for _, e := range env.tm.entries {
			e.timer.Stop()
		}
		for _, a := range env.tm.accounts {
			cls = append(cls, a.cl)
		}
		env.tm.mux.Unlock()
		for _, cl := range cls {
			cl.Close()
		}
		tc.Close()
	})
	return env
}

// setRecords makes the store find those uids' records, each with the one tier
// {{trk.URL(key)}}, and no record (found=false) for the rest.
func (e *privateEnv) setRecords(recs ...testRecord) {
	e.store.setRecord(func(in *ts.GetPrivateRecordRequest) (*ts.GetPrivateRecordReply, error) {
		for _, r := range recs {
			if in.GetUid() == r.uid {
				return &ts.GetPrivateRecordReply{Found: true, Tiers: []*ts.Tier{{Urls: []string{e.trk.URL(r.key)}}}}, nil
			}
		}
		return &ts.GetPrivateRecordReply{}, nil
	})
}

func (e *privateEnv) defaultClient(t *testing.T) *torrent.Client {
	t.Helper()
	cl, err := e.tm.tc.Get()
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// accountTorrents is the number of torrents in every account Client.
func (e *privateEnv) accountTorrents() int {
	e.tm.mux.Lock()
	defer e.tm.mux.Unlock()
	n := 0
	for _, a := range e.tm.accounts {
		n += len(a.cl.Torrents())
	}
	return n
}
