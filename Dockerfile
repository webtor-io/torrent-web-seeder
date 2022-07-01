FROM alpine:latest as certs

# getting certs
RUN apk update && apk upgrade && apk add --no-cache ca-certificates

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
RUN  cd ./server && go build -ldflags '-w -s' -a -installsuffix cgo -o server

FROM alpine:latest

# copy our static linked library
COPY --from=build /app/server/server .

# copy certs
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENV DATA_DIR /data

# tell we are exposing our service on ports 50051 8080 8081 8082
EXPOSE 50051 8080 8081 8082

# run it!
CMD ["./server"]
