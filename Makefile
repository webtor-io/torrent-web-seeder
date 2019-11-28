protoc:
	protoc -I torrent-web-seeder/ torrent-web-seeder/torrent-web-seeder.proto --go_out=plugins=grpc:torrent-web-seeder