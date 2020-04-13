docker build . -t torrent-web-seeder:latest &&
telepresence --new-deployment torrent-web-seeder-debug --expose 8080 --expose 8081 --expose 50051 --docker-run -e INFO_HASH=08ada5a7a6183aae1e09d831df6748d566095a10 -p 8080:8080 -p 8081:8081 -p 50051:50051 -it torrent-web-seeder:latest
