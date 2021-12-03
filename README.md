# torrent-web-seeder

It is a wrapper around BitTorrent-client that introduces additional features:
1. Web-access to the downloading torrent
2. Status GRPC-service
3. Fetching torrent-files from the remote TorrentStore

## Basic usage

```
% ./server help
NAME:
   torrent-web-seeder - Seeds torrent files

USAGE:
   server [global options] command [command options] [arguments...]

VERSION:
   0.0.1

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --probe-host value          probe listening host
   --probe-port value          probe listening port (default: 8081)
   --host value                listening host
   --port value                http listening port (default: 8080)
   --grace value               grace in seconds (default: 600) [$GRACE]
   --download-rate value       download rate [$DOWNLOAD_RATE]
   --torrent-store-host value  torrent store host [$TORRENT_STORE_SERVICE_HOST, $TORRENT_STORE_HOST]
   --torrent-store-port value  torrent store port (default: 50051) [$TORRENT_STORE_SERVICE_PORT, $TORRENT_STORE_PORT]
   --stat-host value           stat listening host
   --stat-port value           stat listening port (default: 50051)
   --info-hash value           torrent infohash [$TORRENT_INFO_HASH, $INFO_HASH]
   --input value               torrent file path
   --help, -h                  show help
   --version, -v               print the version
```