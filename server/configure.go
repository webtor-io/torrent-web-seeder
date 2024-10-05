package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	s "github.com/webtor-io/torrent-web-seeder/server/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}
	app.Flags = cs.RegisterProbeFlags([]cli.Flag{})
	app.Flags = cs.RegisterPprofFlags(app.Flags)
	app.Flags = cs.RegisterPromFlags(app.Flags)
	app.Flags = s.RegisterWebFlags(app.Flags)
	app.Flags = s.RegisterTorrentClientFlags(app.Flags)
	app.Flags = s.RegisterTorrentStoreFlags(app.Flags)
	app.Flags = s.RegisterFileStoreFlags(app.Flags)
	app.Flags = s.RegisterStatFlags(app.Flags)
	// app.Flags = s.RegisterTorrentClientPoolFlags(app.Flags)
	app.Action = run
}

func run(c *cli.Context) error {
	// Setting TorrentStore
	torrentStore := s.NewTorrentStore(c)
	defer torrentStore.Close()

	// Setting TorrentClient
	torrentClient, err := s.NewTorrentClient(c)
	if err != nil {
		return err
	}
	defer torrentClient.Close()

	// Setting TorrentStoreMap
	torrentStoreMap := s.NewTorrentStoreMap(torrentStore)

	// Setting FileStoreMap
	fileStoreMap := s.NewFileStoreMap(c)

	// Setting TouchMap
	touchMap := s.NewTouchMap(c)

	// Setting TorrentMap
	torrentMap := s.NewTorrentMap(torrentClient, torrentStoreMap, fileStoreMap, touchMap)

	// Setting Stat
	stat := s.NewStat(torrentMap)

	// Setting StatGRPC
	statGRPC := s.NewStatGRPC(c, stat)

	// Setting StatWeb
	statWeb := s.NewStatWeb(stat)

	// Setting WebSeeder
	webSeeder := s.NewWebSeeder(torrentMap, statWeb)

	// Setting Web
	web := s.NewWeb(c, webSeeder)
	defer web.Close()

	// Setting Probe
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting Prom
	prom := cs.NewProm(c)
	defer prom.Close()

	// Setting Pprof
	pprof := cs.NewPprof(c)
	defer pprof.Close()

	// Setting Serve
	serve := cs.NewServe(web, probe, prom, pprof, statGRPC)

	// And SERVE!
	err = serve.Serve()

	if err != nil {
		log.WithError(err).Error("got server error")
	}

	return err
}
