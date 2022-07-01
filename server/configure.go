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
	app.Flags = cs.RegisterProbeFlags([]cli.Flag{})
	app.Flags = cs.RegisterS3ClientFlags(app.Flags)
	app.Flags = cs.RegisterPprofFlags(app.Flags)
	app.Flags = s.RegisterWebFlags(app.Flags)
	app.Flags = s.RegisterTorrentClientFlags(app.Flags)
	app.Flags = s.RegisterTorrentStoreFlags(app.Flags)
	app.Flags = s.RegisterStatFlags(app.Flags)
	app.Flags = s.RegisterMetaInfoFlags(app.Flags)
	app.Flags = s.RegisterSnapshotFlags(app.Flags)
	app.Flags = s.RegisterTorrentFlags(app.Flags)
	app.Action = run
}

func run(c *cli.Context) error {
	defer func() {
		log.Info("shuting down... at last!")
	}()

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
	torrent := s.NewTorrent(c, torrentClient, metainfo)

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
	stat := s.NewStat(torrent)

	// Setting StatGRPC
	statGRPC := s.NewStatGRPC(c, stat)

	// Setting StatWeb
	statWeb := s.NewStatWeb(stat)

	// Setting BucketPool
	bp := s.NewBucketPool()

	// Setting WebSeeder
	webSeeder := s.NewWebSeeder(torrent, counter, bp, statWeb)

	// Setting Web
	web := s.NewWeb(c, webSeeder)
	defer web.Close()

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting Pprof
	pprof := cs.NewPprof(c)
	defer pprof.Close()

	// Setting Serve
	serve := s.NewServe(web, statGRPC, probe, torrent, snapshot, pprof)

	// And SERVE!
	err = serve.Serve()

	if err != nil {
		log.WithError(err).Error("got server error")
	}

	return err
}
