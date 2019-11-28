FROM golang:latest

# copy the source files
COPY . /go/src/bitbucket.org/vintikzzzz/torrent-web-seeder

WORKDIR /go/src/bitbucket.org/vintikzzzz/torrent-web-seeder/server

# enable modules
ENV GO111MODULE=on

# disable crosscompiling
ENV CGO_ENABLED=0

# compile linux only
ENV GOOS=linux

# build the binary with debug information removed
RUN go build -mod=vendor -ldflags '-w -s' -a -installsuffix cgo -o server

FROM scratch

# copy our static linked library
COPY --from=0 /go/src/bitbucket.org/vintikzzzz/torrent-web-seeder/server/server .

# tell we are exposing our service on ports 50051 8080 8081
EXPOSE 50051 8080 8081

# run it!
CMD ["./server"]
