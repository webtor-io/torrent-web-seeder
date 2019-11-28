package main

import (
	cs "bitbucket.org/vintikzzzz/common-services"
	s "bitbucket.org/vintikzzzz/torrent-web-seeder/server/services"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}
	cs.RegisterProbeFlags(app)
	s.RegisterWebFlags(app)
	s.RegisterTorrentClientFlags(app)
	s.RegisterTorrentStoreFlags(app)
	s.RegisterStatFlags(app)
	s.RegisterMetaInfoFlags(app)
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
	defer torrentClient.Close()

	// Setting Torrent
	torrent := s.NewTorrent(torrentClient, metainfo)

	// Setting Stat
	stat := s.NewStat(c, torrent)

	// Setting WebSeeder
	webSeeder := s.NewWebSeeder(torrent)

	// Setting Web
	web := s.NewWeb(c, webSeeder)
	defer web.Close()

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting Serve
	serve := s.NewServe(web, stat, probe, torrent)

	// And SERVE!
	err = serve.Serve()
	if err != nil {
		log.WithError(err).Error("Got server error")
	}

	return err
}
