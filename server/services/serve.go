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
	st *StatGRPC
	pr *cs.Probe
	t  *Torrent
	ss *Snapshot
}

func NewServe(w *Web, st *StatGRPC, pr *cs.Probe, t *Torrent, ss *Snapshot) *Serve {
	return &Serve{
		w:  w,
		st: st,
		pr: pr,
		t:  t,
		ss: ss,
	}
}

func (s *Serve) Serve() error {

	webError := make(chan error, 1)
	probeError := make(chan error, 1)
	statError := make(chan error, 1)
	torrentError := make(chan error, 1)
	snapshotError := make(chan error, 1)

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
			err := s.ss.Start()
			if err != nil {
				snapshotError <- err
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
		log.Info("no activity for a grace period")
	case sig := <-sigs:
		log.WithField("signal", sig).Info("got syscall")
	case err := <-webError:
		return errors.Wrap(err, "got Web error")
	case err := <-probeError:
		return errors.Wrap(err, "got Probe error")
	case err := <-statError:
		return errors.Wrap(err, "got Stat error")
	case err := <-torrentError:
		return errors.Wrap(err, "failed to fetch torrent")
	case err := <-snapshotError:
		return errors.Wrap(err, "got snapshot error")
	}
	return nil
}
