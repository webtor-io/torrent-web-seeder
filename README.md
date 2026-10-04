# torrent-web-seeder

BitTorrent client with HTTP interface for streaming torrent content. Part of the [Webtor](https://github.com/webtor-io) platform.

Built on [anacrolix/torrent](https://github.com/anacrolix/torrent) (custom [fork](https://github.com/webtor-io/torrent)) with mmap-based storage, LRU piece eviction, and Prometheus instrumentation.

## Features

- **HTTP file streaming** — serve any file from a torrent over HTTP with range request support
- **gRPC status service** — real-time download progress, piece states, peer counts via `Stat`/`StatStream`/`Files` RPCs
- **Remote torrent store** — fetch `.torrent` metadata from a gRPC [torrent-store](https://github.com/webtor-io/torrent-store) service
- **Vault integration** — redirect to pre-cached files on S3 when available
- **Memory-mapped storage** — mmap-backed piece storage with per-torrent LRU cache eviction; files open on first touch with a bounded number kept open per torrent (`MAX_OPEN_FILES_PER_TORRENT`, default 2048), files under `MMAP_MIN_FILE_SIZE` (64 KB) are read with pread instead of a mapping
- **Diagnostics CLI** — `diagnose` command for troubleshooting torrent download issues

## Architecture

```
                    ┌─────────────────┐
                    │  torrent-store  │ (gRPC, port 50051)
                    │  .torrent files │
                    └────────┬────────┘
                             │
┌──────────┐    HTTP    ┌────▼────────────────────┐    BitTorrent
│  Client  │◄──────────►│  torrent-web-seeder     │◄──────────────► Peers
│          │  :8080     │                         │
└──────────┘            │  ┌───────────────────┐  │
                        │  │ anacrolix/torrent  │  │
                        │  │ mmap storage + LRU │  │
┌──────────┐   gRPC     │  └───────────────────┘  │
│  Proxy   │◄──────────►│                         │
│          │  :50051    │  Prometheus metrics      │──► :8083
└──────────┘            │  Health probes           │──► :8081
                        │  pprof                   │──► :8082
                        └─────────────────────────┘
```

## Usage

### Server mode (default)

```bash
torrent-web-seeder \
  --port 8080 \
  --data-dir /data \
  --torrent-store-host torrent-store \
  --torrent-store-port 50051 \
  --use-stat --use-probe --use-prom
```

Serves torrent content over HTTP:

```
GET /<info-hash>/                  — file listing
GET /<info-hash>/<path>            — stream file (supports Range)
GET /<info-hash>/source.torrent    — download .torrent metadata
GET /<info-hash>/<path>?stats      — download progress page
```

Torrent metadata is resolved from local files (`--input`) or remote torrent-store (gRPC).

### Diagnose mode

Troubleshoot why a torrent isn't downloading — test tracker responses, peer discovery, and download capability:

```bash
torrent-web-seeder diagnose --timeout 60s "magnet:?xt=urn:btih:..."
torrent-web-seeder diagnose ./path/to/file.torrent
```

Example output:

```
=== Torrent Diagnostics ===

--- Client Initialization ---
[OK]   Client started
       Listen: 0.0.0.0:42069
       DHT: 2 server(s)

--- Metadata Resolution ---
[OK]   Metadata received in 0.9s
       Name: Sintel
       Size: 123.3 MB

--- Download Test ---
          6s  peers=3   seeders=3   downloaded=1.0 MB    speed=357.3 KB/s
         12s  peers=6   seeders=6   downloaded=33.1 MB   speed=9.0 MB/s

--- Tracker Status ---
[OK]   udp4://explodie.org:6969           — 32 peers
[FAIL] udp4://tracker.leechers-paradise.org:6969 — no such host

=== Diagnosis ===
[OK] Torrent appears healthy.
     Peers: 6 active, 6 seeders
     Downloaded: 123.8 MB useful data
```

Diagnose flags: `--timeout`, `--http-proxy` (test from different IP), `--torrent-client-debug` (verbose logging), plus all torrent client flags.

## gRPC API

Defined in [`proto/torrent-web-seeder.proto`](proto/torrent-web-seeder.proto):

| Method | Description |
|--------|-------------|
| `Stat(path)` | Point-in-time snapshot: total/completed bytes, peers, seeders, leechers, status, piece states |
| `StatStream(path)` | Server-streaming updates: checked every 1 s, sent on change; the data behind it refreshes at most once a second per path (about every 2 s for a lone subscriber, see below) |
| `Files()` | List all files in the torrent |

Status values: `INITIALIZATION`, `SEEDING`, `IDLE`, `TERMINATED`, `WAITING_FOR_PEERS`, `RESTORING`, `BACKINGUP`.

### Swarm availability

A torrent can be stuck with peers connected: every peer holds a partial copy
and nobody holds the pieces being read. Live stat replies say which of the
scope's pieces (the torrent, the file, or the directory's piece span — the
same pieces as `pieces`, at the same positions) the seeder could get at all.
Cold replies (`live: false`) leave these zero.

"Has" here means "has announced": a peer's claims are all the protocol shows.
A BEP 16 super-seeder announces nothing up front and reveals pieces one HAVE
at a time, so a full copy behind it shows as holes that fill as data arrives;
steady progress with `wanted_missing > 0` is not "stuck".

| Field (JSON key) | Type | Meaning |
|---|---|---|
| `availability` | float (0..1) | Fraction of the scope's pieces complete here or claimed by at least one connected peer. `1` when a connected peer or a web seed claims every piece. A lower bound while `availability_known` is false. |
| `availability_known` | bool | False while the connected peers' piece sets may not all be in (see below). |
| `missing` | `[{start, end}]` | Pieces neither complete here nor claimed by any connected peer, sorted half-open runs of positions. Empty unless `availability_known`. At most 512 runs: past that, the runs separated by the smallest gaps are merged, so a merged run also covers pieces complete here or claimed by a peer — any number of them (64k pieces with one in eight missing at random: merged stretches of up to ~20). Draw completion (`pieces[].complete`) over the hatch. |
| `wanted_missing` | int | Wanted pieces (priority above `NONE`) that are not complete and have no source among the connected peers. `0` unless `availability_known`. Includes sticky priorities nobody may be waiting on: a warm-up's `HIGH` stays until the piece completes or the torrent unloads, a closed reader's window stays `NORMAL` for `--reader-linger` (90 s), and the whole-torrent reply counts every file's. |
| `reader_missing` | int | Of those, the pieces an open reader (a stream or download in progress) is on or reading ahead into (priority `READAHEAD` or above). Above `0`: a reader is or soon will be waiting on a piece no connected peer has. `0` unless `availability_known`. |
| `missing_unchanged` | bool | StatStream frames only: `true` when the frame leaves `missing` out because it equals the last frame's — keep the runs you have. `false` in every frame that carries `missing` (including one where it became empty) and in `Stat` replies. |

In a StatStream frame `availability`, `availability_known`, `wanted_missing`
and `reader_missing` are always whole; `missing` is whole when present (it
replaces, it is not a diff) and is present only when it changed, so a frame
sent because pieces completed does not repeat up to 512 runs (~14.6 KB
against ~0.3 KB for a one-piece diff frame at the cap). A change in any of
these fields alone sends a frame.

**When availability is known.** A connection joins the torrent right after the
BitTorrent handshake, before the peer's first message; its piece set is empty
until that message (BITFIELD, HAVE_ALL or HAVE_NONE), and "lazy bitfield"
clients send part of it as HAVEs after. So an early union paints holes that
are not there. The rule:

- known at once when a source claims every piece: a connected seeder
  (`ConnectedSeeders > 0`: its claim to every piece is known and cannot
  grow), or a web seed that serves the whole torrent (below);
- known at once when nothing in the scope is missing — every piece complete
  here or already claimed; more announcements can only add. A cached file on
  a dead swarm reads known, `availability` 1;
- otherwise known once the torrent has had connected peers for **20 s**
  without a break (`availabilitySettle`), measured by the torrent's watcher
  in `TorrentMap` every 50 ms. The clock is the torrent's: a chain of
  short-lived connections with no moment at zero peers counts as one streak,
  and a peer that joins later is unread for its round trip. 20 s is a product
  delay covering the first seconds' burst of connections, not a protocol
  constant. The library does expose per-connection moments
  (`Callbacks.CompletedHandshake`, `PeerConnAdded`, and `ReadMessage` for the
  first message), and "every connection has sent its first message" would be
  exact; it is not used because `ReadMessage` runs under the client lock for
  every message on the data path, and under churn at the connection limit
  there is nearly always a connection younger than a round trip, so it would
  stay unknown exactly when a stuck swarm cycles peers;
- a web seed: the library shows its piece set as a count
  (`Peer.Stats().RemotePieceCount`), which is every piece once the info is
  known, or none for a directory URL without a trailing `/` (the library never
  requests from it). A whole-torrent web seed counts as a seeder — also when
  its URL is dead, the way a peer's HAVE_ALL can be false and the library
  itself counts it into piece availability. An empty one is ignored. One with
  part of the torrent (not produced by the current library) makes the result
  unknown unless nothing is missing. Prod runs with `DISABLE_WEBSEEDS`;
  self-hosted does not.

Known means the connected peers' pieces are in, not that the swarm has been
found: peers keep arriving after 20 s (prod, 7 days to 2026-09-24: first peer
p50 3.9 s after add, ten peers p50 37 s, for the 32% of torrents with a peer that reach ten),
and availability rises as they do.

**Refresh cadence.** Stat replies are cached 1 s per path and the swarm
snapshot 1 s per torrent. The cache's expiry starts when a computation ends
and StatStream's 1 s ticker is phase-locked to its first computation, so the
tick after a computation still hits the cache and the next recomputes: a lone
subscriber gets fresh numbers every ~2 s; subscribers or paths out of phase
get up to one computation a second. With a snapshot possibly taken up to 1 s
earlier by another path, `availability_known` flips 20–23 s after the first
peer, and a hole a new peer fills can show for up to ~3 s.

**Cost.** Computed only on the stat path, i.e. only while someone watches;
the peers are read at most once a second per torrent into one union of their
piece sets, shared by every path asked about the torrent, and each path's
computation then walks only its own scope (roaring `AndNot` of the scope
against the union, then the pieces left). With a seeder connected nothing
extra is read. Without one, reading goes through the library's public API:
`WebseedPeerConns`, `Peer.Stats` per web seed, `PeerConns`, and
`PeerConn.PeerPieces` per connection — each a read lock on the client lock
(one RWMutex for every torrent on the pod), `PeerPieces` a clone of the peer's
bitmap made under it. The clone is folded into the union after the lock is
released and dropped, so lock hold times are the clone's, and a snapshot holds
one bitmap however many peers there are. Measured on an M3 Pro
(`BenchmarkStatCost`: 16 real loopback peers each claiming a random half, 17–23
connections; `BenchmarkComputeAvailability`: synthetic peers claiming random
halves of three quarters of the torrent, the last quarter in one-piece holes):

| | 8 192 pieces | 65 536 pieces |
|---|---|---|
| peer read per connection: lock + clone + fold into the union (real) | 4.5 µs | 1.4 µs |
| union of 50 / 80 / 300 synthetic peers, once per torrent a second | 0.28 / 0.45 / 1.6 ms | 0.04 / 0.07 / 0.24 ms |
| per path from the union, real peers: whole torrent / 200-piece file | 3.3 / 2.2 µs | 4.5 / 2.4 µs |
| per path from the union, synthetic holes: whole torrent / 200-piece file | 0.15 ms / 4 µs | 1.3 ms / 4 µs |
| per 200-piece file before the union was shared (one set per peer), 80 peers | 0.77 ms | 0.30 ms |
| for comparison: `torrentStat`'s existing per-piece `Piece.State` loop | 0.30 ms, 8 192 read locks | 2.4 ms, 65 536 read locks |

The 1.3 ms whole-torrent case is the 512-run cap sorting ~15 000 one-piece
holes; real holes are mostly long runs. Clones are ~8 KB per connection at
both sizes (array containers at 8 192 pieces make the fold the dearer part
there). At prod's 80 connections per torrent a snapshot is 82 read-lock
acquisitions; with a lone subscriber that is about 41 a second per watched
torrent without a seeder, 82 at most — a fraction of the one read lock per
piece the whole-torrent stat already takes.

## Configuration

All configuration via CLI flags and environment variables.

### Server flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--host` | `WEB_HOST` | — | HTTP listen host |
| `--port` | `WEB_PORT` | `8080` | HTTP listen port |
| `--data-dir` | `DATA_DIR` | system temp | Storage directory for torrent data |
| `--input` | `INPUT` | — | Local `.torrent` file or directory |
| `--torrent-store-host` | `TORRENT_STORE_SERVICE_HOST` | — | Remote torrent-store gRPC host |
| `--torrent-store-port` | `TORRENT_STORE_SERVICE_PORT` | `50051` | Remote torrent-store gRPC port |
| `--max-readahead` | `MAX_READAHEAD` | `20MB` | Read-ahead buffer size |

- `--reader-linger` / `READER_LINGER` (default `90s`) — after a file request
  ends, keep the pieces of the reader's window `[pos, pos+max-readahead)`
  at Normal priority this long, then release the ones still at Normal and
  still incomplete. anacrolix derives piece priorities from readers that are
  actively reading (an idle reader wants nothing), so a client disconnect
  otherwise abandons the piece at once, and a client whose idle timeout is
  shorter than the swarm's time-to-first-byte (download managers: 30 s)
  retries the same offset forever. Pieces at a higher explicit priority
  (warmup's High) are left alone. Bounded to 512 windows per pod; `0`
  disables.

- Warmup (`?warmup=true`, optional `Range`) takes a file or a directory;
  a directory is warmed as its files concatenated in torrent order, which
  is the byte order of the archive built from it.
- Stats look but do not touch (2026-09-09). `Stat`/`StatStream` ask the
  client whether it already holds the torrent (`TorrentMap.Peek`); if it
  does — someone is streaming, downloading or warming it — the numbers are
  live and `live: true`. Otherwise the reply is **cold**: metainfo from the
  file/torrent store, completed pieces from the per-torrent `.torrent.db`,
  peers zero, `live: false`; the torrent is not loaded, the swarm is not
  joined, the TTL is not extended. A status stream switches to live by
  itself once the torrent comes up. Before this, every page view joined the
  swarm on the viewer's behalf.
- Stats (`?stats=true`, gRPC `Stat`/`StatStream`) answer for the whole
  torrent (empty path), a single file, or a **directory** — the files under
  it summed, pieces over the span they occupy. Before 2026-09-05 a directory
  path was a NotFound, and every single-root-directory torrent's resource
  page (which asks for the root item) got a 500 and a "status unavailable"
  badge — about 2 000 torrents a day.

### Torrent client flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--download-rate` | `DOWNLOAD_RATE` | unlimited | Download rate limit (e.g. `100MB`) |
| `--per-torrent-cache-budget` | `PER_TORRENT_CACHE_BUDGET` | `50GB` | LRU cache per torrent |
| `--established-conns-per-torrent` | `ESTABLISHED_CONNS_PER_TORRENT` | — | Max active peers per torrent |
| `--http-proxy` | `HTTP_PROXY` | — | HTTP proxy for tracker/webseed requests |
| `--no-upload` | `NO_UPLOAD` | `false` | Disable uploading |
| `--seed` | `SEED` | `false` | Continue seeding after download |
| `--disable-utp` | `DISABLE_UTP` | `false` | Disable uTP protocol |
| `--disable-webtorrent` | `DISABLE_WEBTORRENT` | `false` | Disable WebTorrent |
| `--disable-webseeds` | `DISABLE_WEBSEEDS` | `false` | Disable webseeds |
| `--torrent-client-debug` | `TORRENT_CLIENT_DEBUG` | `false` | Verbose torrent client logging |

### Infrastructure flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--use-stat` | `USE_STAT` | `false` | Enable gRPC stat service (port 50051) |
| `--use-probe` | `USE_PROBE` | `false` | Enable health probe (port 8081) |
| `--use-pprof` | `USE_PPROF` | `false` | Enable pprof (port 8082) |
| `--use-prom` | `USE_PROM` | `false` | Enable Prometheus metrics (port 8083) |

## Docker

```bash
docker build -t torrent-web-seeder .
docker run -p 8080:8080 -v /data:/data torrent-web-seeder
```

Ports: `8080` (HTTP), `50051` (gRPC), `8081` (probes), `8082` (pprof), `8083` (Prometheus).

## Development

```bash
# Build
cd server && go build -o server

# Regenerate protobuf
make protoc

# Run with local torrent file
./server/server --input ./torrents/Sintel.torrent --data-dir /tmp/tws

# Run diagnostics
./server/server diagnose "magnet:?xt=urn:btih:..."
```

## Metrics

Prometheus metrics exported on `:8083/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `torrent_web_seeder_dial_attempts_total` | Counter | Dial attempts by type (peer/http/tracker) |
| `torrent_web_seeder_dial_failures_total` | Counter | Dial failures by type and error category |
| `torrent_web_seeder_dial_success_total` | Counter | Successful dials by type |
| `torrent_web_seeder_handshake_success_total` | Counter | Successful peer handshakes |
| `torrent_web_seeder_established_connections` | Gauge | Active peer connections |
| `torrent_web_seeder_half_open_connections` | Gauge | Pending peer connections |
| `torrent_web_seeder_active_torrents_count` | Gauge | Number of active torrents |
| `torrent_web_seeder_time_to_first_peer_ms` | Histogram | Latency to first peer connection |
| `torrent_web_seeder_time_to_first_byte_ms` | Histogram | Latency to first downloaded byte |
| `torrent_web_seeder_stall_*_seconds_total` | Counter | Stall detection (discovery/idle/download) |

## License

MIT
