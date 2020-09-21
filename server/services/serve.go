package services

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"

	cs "github.com/webtor-io/common-services"
)

type Serve struct {
	w  *Web
	st *Stat
	pr *cs.Probe
	t  *Torrent
	ss *Snapshot
	fd *FullDownload
}

func NewServe(w *Web, st *Stat, pr *cs.Probe, t *Torrent, ss *Snapshot, fd *FullDownload) *Serve {
	return &Serve{w: w, st: st, pr: pr, t: t, ss: ss, fd: fd}
}

func (s *Serve) Serve() error {

	webError := make(chan error, 1)
	probeError := make(chan error, 1)
	statError := make(chan error, 1)
	torrentError := make(chan error, 1)
	snapshotError := make(chan error, 1)
	fullDownloadError := make(chan error, 1)

	go func() {
		err := s.w.Serve()
		webError <- err
	}()

	go func() {
		err := s.pr.Serve()
		probeError <- err
	}()

	go func() {
		err := s.st.Serve()
		statError <- err
	}()
	go func() {
		_, err := s.t.Get()
		if err != nil {
			torrentError <- err
		}
	}()
	if s.ss != nil {
		go func() {
			snapshotError <- s.ss.Start()
		}()
		go func() {
			err := s.fd.Start()
			if err != nil {
				fullDownloadError <- err
			}
		}()
	}
	expire, err := s.w.Expire()
	if err != nil {
		return err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-expire:
		log.Info("No activity for a grace period")
	case sig := <-sigs:
		log.WithField("signal", sig).Info("Got syscall")
	case err := <-webError:
		return errors.Wrap(err, "Got Web error")
	case err := <-probeError:
		return errors.Wrap(err, "Got Probe error")
	case err := <-statError:
		return errors.Wrap(err, "Got Stat error")
	case err := <-torrentError:
		return errors.Wrap(err, "Failed to fetch torrent")
	case err := <-snapshotError:
		if err != nil {
			return errors.Wrap(err, "Got snapshot error")
		}
		log.Info("All pieces uploaded")
	case err := <-fullDownloadError:
		return errors.Wrap(err, "Got full download error")
	}
	return nil
}
