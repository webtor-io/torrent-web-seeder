protoc:
	protoc proto/torrent-web-seeder.proto --go_out=. --go_opt=paths=source_relative \
		   --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/torrent-web-seeder.proto
		   gsed -i 's/,omitempty//' ./proto/torrent-web-seeder.pb.go