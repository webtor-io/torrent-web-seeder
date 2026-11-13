package services

import (
	"time"

	"github.com/anacrolix/torrent"
)

// accountClient is one account's BitTorrent Client and the number of its
// torrents the map holds.
type accountClient struct {
	cl       *torrent.Client
	torrents int
	idle     *time.Timer
}

// accountFor returns uid's Client, built from the pod's settings (forUser) on
// the account's first torrent. Called with s.mux held.
func (s *TorrentMap) accountFor(uid string) (*accountClient, error) {
	if a, ok := s.accounts[uid]; ok {
		return a, nil
	}
	cl, err := s.tc.forUser().get()
	if err != nil {
		return nil, err
	}
	a := &accountClient{cl: cl}
	s.accounts[uid] = a
	return a, nil
}

// releaseAccount counts out one of uid's torrents. Called with s.mux held.
func (s *TorrentMap) releaseAccount(uid string) {
	if a, ok := s.accounts[uid]; ok {
		a.torrents--
	}
}
