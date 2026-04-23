# torrent-web-seeder

A wrapper around the BitTorrent client ([anacrolix/torrent](https://github.com/anacrolix/torrent)) that provides:
1. HTTP access to torrent content (file streaming with range requests)
2. gRPC status service (real-time download progress, piece states)
3. Fetching torrent files from a remote TorrentStore
4. Torrent diagnostics CLI for troubleshooting download issues

## Diagnostics

The `diagnose` command helps troubleshoot why a torrent isn't downloading. Pass a magnet URI or `.torrent` file and get a structured report covering tracker responses, peer discovery, and download capability.

```
torrent-web-seeder diagnose [options] <magnet-uri or .torrent path>
```

### Example

```
$ torrent-web-seeder diagnose --timeout 60s "magnet:?xt=urn:btih:08ada5a7..."

=== Torrent Diagnostics ===

--- Client Initialization ---
[OK]   Client started
       Listen: 0.0.0.0:42069
       DHT: 2 server(s)

--- Torrent Input ---
       Type: magnet link
       Info Hash: 08ada5a7a6183aae1e09d831df6748d566095a10
       Display Name: Sintel
       Trackers: 8

--- Metadata Resolution ---
[OK]   Metadata received in 0.9s
       Name: Sintel
       Size: 123.3 MB

--- Download Test ---
          6s  peers=3   seeders=3   downloaded=1.0 MB    speed=357.3 KB/s
         12s  peers=6   seeders=6   downloaded=33.1 MB   speed=9.0 MB/s
[OK]   First useful data at 6.0s

--- Tracker Status ---
[OK]   udp4://explodie.org:6969           — 32 peers
[OK]   udp4://tracker.empire-js.us:1337   — 12 peers
[FAIL] udp4://tracker.leechers-paradise.org:6969 — no such host

=== Diagnosis ===
[OK] Torrent appears healthy.
     Peers: 6 active, 6 seeders
     Downloaded: 123.8 MB useful data
```

### Diagnostic phases

| Phase | What it checks |
|-------|---------------|
| Client Initialization | Torrent client startup, listen addresses, DHT servers |
| Torrent Input | Magnet URI / .torrent parsing, info hash, tracker list |
| Metadata Resolution | Ability to obtain torrent metadata from peers (with live progress) |
| Download Test | Actual data download — peer count, speed, first byte latency |
| Tracker Status | Per-tracker announce results — peers returned, errors, failure reasons |
| Final Statistics | Aggregate stats — peers, seeders, bytes downloaded, pieces |
| Connected Peers | Per-peer detail — address, download rate, useful data |
| Raw Client Status | Full `WriteStatus` dump from anacrolix/torrent for deep debugging |
| Diagnosis | Automated verdict with root cause analysis and suggestions |

### Options

| Flag | Description | Default |
|------|-------------|---------|
| `--timeout` | Total diagnostic timeout | `60s` |
| `--torrent-client-debug` | Enable verbose anacrolix/torrent logging | off |
| `--http-proxy` | Route traffic through HTTP proxy (test from different IP) | — |
| `--data-dir` | Temp storage directory | system temp |
| `--disable-utp` | Disable uTP protocol | off |
| `--disable-webtorrent` | Disable WebTorrent | off |
| `--disable-webseeds` | Disable webseeds | off |

All torrent client flags from the main server mode are also available.

### Common diagnoses

- **All trackers failed** — tracker blocked your IP, torrent removed from tracker, or tracker is down
- **No peers discovered** — dead torrent, IP blocked, or DHT/PEX not finding peers
- **Peers found but no seeders** — only leechers available, torrent partially seeded
- **Seeders connected but no data** — seeders choking, rate limit too restrictive

## Server mode

Default mode — runs the HTTP server for torrent content streaming:

```
torrent-web-seeder [global options]
```

Key options: `--host`, `--port` (default 8080), `--input` (torrent file path), `--torrent-store-host/port` (remote torrent store), `--download-rate`, `--data-dir`.
