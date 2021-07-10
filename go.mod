module github.com/webtor-io/torrent-web-seeder

go 1.16

require (
	code.cloudfoundry.org/bytefmt v0.0.0-20210608160410-67692ebc98de
	github.com/RoaringBitmap/roaring v0.9.1 // indirect
	github.com/anacrolix/chansync v0.0.0-20210623023814-793ff0e2924e // indirect
	github.com/anacrolix/log v0.9.0
	github.com/anacrolix/sync v0.4.0 // indirect
	github.com/anacrolix/torrent v1.28.1-0.20210702044313-e1cac00bd5da
	github.com/aws/aws-sdk-go v1.39.4
	github.com/bakins/logrus-middleware v0.0.0-20180426214643-ce4c6f8deb07
	github.com/bakins/test-helpers v0.0.0-20141028124846-af83df64dc31 // indirect
	github.com/dustin/go-humanize v1.0.0
	github.com/go-pg/pg/v10 v10.10.1 // indirect
	github.com/golang/protobuf v1.5.2
	github.com/google/go-cmp v0.5.6 // indirect
	github.com/gosuri/uilive v0.0.4 // indirect
	github.com/gosuri/uiprogress v0.0.1
	github.com/grpc-ecosystem/go-grpc-middleware v1.3.0
	github.com/joonix/log v0.0.0-20200409080653-9c1d2ceb5f1d
	github.com/juju/ratelimit v1.0.1
	github.com/mattn/go-isatty v0.0.13 // indirect
	github.com/pion/webrtc/v3 v3.0.31 // indirect
	github.com/pkg/errors v0.9.1
	github.com/sirupsen/logrus v1.8.1
	github.com/urfave/cli v1.22.5
	github.com/vmihailenco/msgpack/v5 v5.3.4 // indirect
	github.com/webtor-io/common-services v0.0.0-20210506124642-57d7ac936cc4
	github.com/webtor-io/gracenet v0.0.0-20200102122601-7e0e6f3c06b5
	github.com/webtor-io/torrent-store v0.0.0-20210710130004-9afe9e0d1ff5
	go.etcd.io/bbolt v1.3.6 // indirect
	golang.org/x/crypto v0.0.0-20210616213533-5ff15b29337e // indirect
	golang.org/x/sys v0.0.0-20210630005230-0f9fa26af87c // indirect
	golang.org/x/time v0.0.0-20210611083556-38a9dc6acbc6
	google.golang.org/genproto v0.0.0-20210708141623-e76da96a951f // indirect
	google.golang.org/grpc v1.39.0
)

// Hack to prevent the willf/bitset module from being upgraded to 1.2.0.
// They changed the module path from github.com/willf/bitset to
// github.com/bits-and-blooms/bitset and a couple of dependent repos are yet
// to update their module paths.
exclude (
	github.com/RoaringBitmap/roaring v0.7.0
	github.com/RoaringBitmap/roaring v0.7.1
	github.com/willf/bitset v1.2.0
)
