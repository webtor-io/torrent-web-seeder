FROM alpine:latest AS certs

# getting certs
RUN apk update && apk upgrade && apk add --no-cache ca-certificates

FROM golang:latest AS build

# set work dir
WORKDIR /app

# copy the source files
COPY . .

# compile linux only
ENV GOOS=linux

ENV CGO_LDFLAGS="-static"

# build the binary with debug information removed; this proto and
# torrent-store's both define FilesRequest/FilesReply without a package,
# so the registry must not panic on the duplicate names at start
RUN  cd ./server && go build -ldflags '-w -s -X google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=ignore' -a -installsuffix cgo -o server

FROM alpine:latest

# copy our static linked library
COPY --from=build /app/server/server .

# copy certs
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENV DATA_DIR=/data

# tell we are exposing our services
EXPOSE 50051 8080 8081 8082 8083

# run it!
CMD ["./server"]
