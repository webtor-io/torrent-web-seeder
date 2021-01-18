package main

import (
	"net/http"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/torrent-web-seeder/server/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}
	cs.RegisterProbeFlags(app)
	s.RegisterWebFlags(app)
	s.RegisterTorrentClientFlags(app)
	s.RegisterTorrentStoreFlags(app)
	s.RegisterStatFlags(app)
	s.RegisterMetaInfoFlags(app)
	s.RegisterSnapshotFlags(app)
	cs.RegisterS3ClientFlags(app)
	app.Action = run
}

func run(c *cli.Context) error {
	// Setting TorrentStore
	torrentStore := s.NewTorrentStore(c)
	defer torrentStore.Close()

	// Setting MetaInfo
	metainfo := s.NewMetaInfo(c, torrentStore)

	// Setting TorrentClient
	torrentClient, err := s.NewTorrentClient(c)
	if err != nil {
		return errors.Wrap(err, "Failed to init TorrentClient")
	}

	// Setting Torrent
	torrent := s.NewTorrent(torrentClient, metainfo)

	// Setting conter
	counter := s.NewCounter()

	// Setting S3 Client
	s3 := cs.NewS3Client(c, &http.Client{
		Timeout: time.Second * 60,
	})

	// Setting Snapshot
	snapshot, err := s.NewSnapshot(c, torrent, counter, s3)
	if err != nil {
		return errors.Wrap(err, "Failed to init Snapshot")
	} else if snapshot != nil {
		defer snapshot.Close()
	}

	// Snapshot should close first of them all
	defer torrentClient.Close()

	// Setting Stat
	stat := s.NewStat(c, torrent)

	// Setting WebSeeder
	webSeeder := s.NewWebSeeder(torrent, counter)

	// Setting Web
	web := s.NewWeb(c, webSeeder)
	defer web.Close()

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting Serve
	serve := s.NewServe(web, stat, probe, torrent, snapshot)

	// And SERVE!
	err = serve.Serve()

	if err != nil {
		log.WithError(err).Error("Got server error")
	}

	return err
}
