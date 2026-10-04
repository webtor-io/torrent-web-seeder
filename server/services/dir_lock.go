package services

import (
	"os"
	"path/filepath"
	"syscall"

	log "github.com/sirupsen/logrus"
)

// A torrent's directory is shared by every seeder pod on the node: the data
// dir is a hostPath, and GetDir names a torrent's directory by infohash
// alone. Two pods that have one torrent loaded at once share its content files
// and .torrent.db, but each keeps its own record of what is on disk: evicted
// flags, the library's piece bitmap, the completion loop's complete files. A
// hole one of them punches is data to the other. 2026-10-02 06:38 glwq2,
// opening 071e75f1 over budget, evicted piece 9093 while spmdr had the torrent
// loaded. The file's file_completion row came back, and every Vault download
// of the file since (35 to 2026-10-04) has come from cache with 4 MiB of
// zeros where the piece was.
//
// So whoever reads a torrent's files holds <dir>/.lock shared while it reads:
// an open storage for its whole life, a cache-path stream for its response
// (only of a torrent nobody has opened with eviction on, FileCacheMap.Open). A
// hole is punched only with the lock held exclusive, that is, when nobody
// else holds the directory. Otherwise the eviction is put off to the next
// MarkComplete or sweep, and the torrent runs over its cache budget for as
// long as it is shared (torrent_web_seeder_cache_evictions_deferred_total).

// lockDir opens dir's lock file and takes it shared. It waits only while an
// eviction holds it exclusive, which is one piece's punch.
func lockDir(dir string) (*os.File, error) {
	f, err := openLockFile(filepath.Join(dir, ".lock"))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// evictGateName is the eviction gate's file (see whileAlone). It exists in
// the dir of every torrent a storage has opened with eviction on, and the
// cache path leaves such torrents to the torrent (FileCacheMap.Open).
const evictGateName = ".evict.lock"

// openEvictGate opens dir's eviction gate, creating it.
func openEvictGate(dir string) (*os.File, error) {
	return openLockFile(filepath.Join(dir, evictGateName))
}

func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
}

// whileAlone runs fn with the torrent's directory held exclusive and reports
// whether it could; false means another holder has the directory and fn did
// not run. fn punches and writes sqlite, and nothing that can wait on the
// network: every other pod's OpenTorrent of the torrent waits for it under
// that pod's client lock.
//
// The storage holds the lock shared itself, and on Linux flock changes a held
// lock by dropping it first: after a failed upgrade the storage holds nothing
// until it takes the lock shared again (macOS keeps the shared lock, so tests
// there cannot see this). The gate makes that gap safe. Only its holder ever
// asks for the lock exclusive, so nobody can take it exclusive in the gap and
// taking it shared again never waits. Two goroutines of one
// storage share its descriptors, which flock does not tell apart, so aloneMu
// keeps them apart.
func (ts *mmapTorrentStorage) whileAlone(fn func()) bool {
	ts.aloneMu.Lock()
	defer ts.aloneMu.Unlock()
	if ts.dirLock == nil { // a storage built by hand in a test
		fn()
		return true
	}
	if ts.dirClosed {
		return false
	}
	gate := int(ts.evictGate.Fd())
	if err := syscall.Flock(gate, syscall.LOCK_EX); err != nil {
		log.WithError(err).Error("failed to take the eviction gate")
		return false
	}
	defer func() { _ = syscall.Flock(gate, syscall.LOCK_UN) }()
	lock := int(ts.dirLock.Fd())
	alone := syscall.Flock(lock, syscall.LOCK_EX|syscall.LOCK_NB) == nil
	if alone {
		fn()
	} else if ts.afterFailedUpgrade != nil {
		ts.afterFailedUpgrade()
	}
	if err := syscall.Flock(lock, syscall.LOCK_SH); err != nil {
		log.WithError(err).Error("failed to hold the torrent dir shared again")
	}
	return alone
}

// unlockDir lets go of the directory. Called last in Close: until then the
// storage may still read the files on the strength of what it remembers.
func (ts *mmapTorrentStorage) unlockDir() {
	ts.aloneMu.Lock()
	defer ts.aloneMu.Unlock()
	if ts.dirLock == nil || ts.dirClosed {
		return
	}
	ts.dirClosed = true
	_ = ts.dirLock.Close()
	if ts.evictGate != nil {
		_ = ts.evictGate.Close()
	}
}
