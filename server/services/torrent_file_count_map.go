package services

import (
	"strings"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/webtor-io/lazymap"
)

type TorrentFileCountMap struct {
	lazymap.LazyMap[*metainfo.Info]
	fsm *FileStoreMap
	tsm *TorrentStoreMap
}

func NewTorrentFileCountMap(fsm *FileStoreMap, tsm *TorrentStoreMap) *TorrentFileCountMap {
	return &TorrentFileCountMap{
		fsm: fsm,
		tsm: tsm,
		LazyMap: lazymap.New[*metainfo.Info](&lazymap.Config{
			Capacity: 100,
		}),
	}
}

func (s *TorrentFileCountMap) getInfo(h string) (*metainfo.Info, error) {
	mi, err := s.fsm.Get(h)
	if err != nil {
		return nil, err
	}
	if mi == nil {
		mi, err = s.tsm.Get(h)
		if err != nil {
			return nil, err
		}
	}
	if mi == nil {
		return nil, nil
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *TorrentFileCountMap) GetInfo(h string) (*metainfo.Info, error) {
	return s.LazyMap.Get(h, func() (*metainfo.Info, error) {
		return s.getInfo(h)
	})
}

// TotalFiles returns total file count for the torrent (1 for single-file torrents).
func (s *TorrentFileCountMap) TotalFiles(h string) (int, error) {
	info, err := s.GetInfo(h)
	if err != nil || info == nil {
		return 0, err
	}
	if len(info.Files) == 0 {
		return 1, nil
	}
	return len(info.Files), nil
}

// DirFileCount returns the number of files under a directory prefix.
func (s *TorrentFileCountMap) DirFileCount(h string, dirPath string) (int, error) {
	info, err := s.GetInfo(h)
	if err != nil || info == nil {
		return 0, err
	}
	count := 0
	for _, f := range info.Files {
		path := info.Name + "/" + strings.Join(f.Path, "/")
		if strings.HasPrefix(path, dirPath+"/") {
			count++
		}
	}
	return count, nil
}
