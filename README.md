# torrent-web-seeder

BitTorrent client with HTTP interface for streaming torrent content. Part of the [Webtor](https://github.com/webtor-io) platform.

Built on [anacrolix/torrent](https://github.com/anacrolix/torrent) (custom [fork](https://github.com/webtor-io/torrent)) with mmap-based storage, LRU piece eviction, and Prometheus instrumentation.

## Features

- **HTTP file streaming** — serve any file from a torrent over HTTP with range request support
- **gRPC status service** — real-time download progress, piece states, peer counts via `Stat`/`StatStream`/`Files` RPCs
- **Remote torrent store** — fetch `.torrent` metadata from a gRPC [torrent-store](https://github.com/webtor-io/torrent-store) service
- **Vault integration** — redirect to pre-cached files on S3 when available
- **Memory-mapped storage** — mmap-backed piece storage with per-torrent LRU cache eviction
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
| `StatStream(path)` | Server-streaming updates (sends on change, 3s interval) |
| `Files()` | List all files in the torrent |

Status values: `INITIALIZATION`, `SEEDING`, `IDLE`, `TERMINATED`, `WAITING_FOR_PEERS`, `RESTORING`, `BACKINGUP`.

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
