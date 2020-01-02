# common-services
Collection of commonly used services at webtor.io

## Probe
Generates standard liveness and readiness probe endpoints for kubernetes

## Serve
Runs simultaneously multiple services in goroutines

## Example usage

```golang
package main

import (
	cs "github.com/webtor-io/common-services"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	s "github.com/webtor-io/torrent-http-proxy/services"
)

func configure(app *cli.App) {
	app.Flags = []cli.Flag{}

	s.RegisterWebFlags(app)
	cs.RegisterProbeFlags(app)

	app.Action = run
}

func run(c *cli.Context) error {
	// Setting ProbeService
	probe := cs.NewProbe(c)
	defer probe.Close()

	// Setting WebService
	web := s.NewWeb(c)
	defer web.Close()

	// Setting ServeService
	serve := cs.NewServe(probe, web)

	// And SERVE!
	err := serve.Serve()
	if err != nil {
		log.WithError(err).Error("Got serve error")
	}
	return err
}
