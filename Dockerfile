FROM restic/restic:latest as restic

FROM golang:latest as build

# set work dir
WORKDIR /app

# copy the source files
COPY . .

# enable modules
ENV GO111MODULE=on

# disable crosscompiling
ENV CGO_ENABLED=0

# compile linux only
ENV GOOS=linux

# build the binary with debug information removed
RUN  cd ./server && go build -mod=vendor -ldflags '-w -s' -a -installsuffix cgo -o server

FROM alpine:latest

# copy certs
COPY --from=restic /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# copy restic
COPY --from=restic /usr/bin/restic /usr/bin/restic

# copy our static linked library
COPY --from=build /app/server/server .

ENV DATA_DIR /data

# tell we are exposing our service on ports 50051 8080 8081
EXPOSE 50051 8080 8081

# run it!
CMD ["./server"]
