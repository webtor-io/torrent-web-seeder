package services

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	logrusmiddleware "github.com/bakins/logrus-middleware"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// warmupTimeout caps how long a single SSE warmup stream can stay open.
// Mirrors the StatStream timeout so a stuck client / dead torrent cannot
// hold the handler goroutine forever.
const warmupTimeout = 30 * time.Minute

type Warmup struct {
	tm *TorrentMap
}

func NewWarmup(tm *TorrentMap) *Warmup {
	return &Warmup{tm: tm}
}

// Serve handles `?warmup` SSE requests. It parses the Range header (or
// defaults to the whole file), bumps PiecePriorityHigh on every piece
// covering that byte range, and emits `data: <downloaded>\n\n` once per
// second where downloaded is the number of bytes within the requested
// range that the seeder has already verified. The stream closes when
// downloaded == total or the client disconnects; the close itself is the
// "warmup complete" signal — the client already knows the requested
// range length.
//
// Piece priorities are intentionally NOT lowered when the client leaves:
// pieces that finish after disconnect remain cached and ready for the
// next opener, which is the whole point of "prewarming".
func (s *Warmup) Serve(w http.ResponseWriter, r *http.Request, h string, p string) error {
	ha, ok := w.(*logrusmiddleware.Handler)
	if !ok {
		return errors.Errorf("unable to get writer")
	}
	flusher, ok := ha.ResponseWriter.(http.Flusher)
	if !ok {
		return errors.Errorf("streaming unsupported")
	}

	t, err := s.tm.Get(r.Context(), h)
	if err != nil {
		return err
	}
	f := findFile(t, p)
	if f == nil {
		http.NotFound(w, r)
		return nil
	}
	if f.Length() <= 0 {
		return errors.Errorf("empty file")
	}

	rangeStart, rangeEnd, err := parseSingleRange(r.Header.Get("Range"), f.Length())
	if err != nil {
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return nil
	}
	total := rangeEnd - rangeStart + 1

	pieceLen := t.Info().PieceLength
	if pieceLen <= 0 {
		return errors.Errorf("piece length is zero")
	}
	firstPieceInFile := int(f.Offset() / pieceLen)

	log.WithFields(log.Fields{
		"hash":  h,
		"path":  p,
		"range": r.Header.Get("Range"),
		"start": rangeStart,
		"end":   rangeEnd,
		"total": total,
	}).Info("serve warmup")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// Mirror StatWeb: nginx-ingress would otherwise buffer the small JSON
	// frames in its 64×128KB proxy buffers and clients would see updates
	// only after a 30s+ flush.
	w.Header().Set("X-Accel-Buffering", "no")

	// Bump priorities on every piece overlapping the requested range.
	// SetPriority is idempotent and the request-strategy layer takes the
	// max across all sources, so this won't downgrade pieces a concurrent
	// reader has already raised to PiecePriorityNow.
	state := f.State()
	var fileOff int64
	for i, ps := range state {
		pieceFileStart := fileOff
		pieceFileEnd := fileOff + ps.Bytes - 1
		fileOff += ps.Bytes
		if pieceFileEnd < rangeStart || pieceFileStart > rangeEnd {
			continue
		}
		t.Piece(firstPieceInFile + i).SetPriority(torrent.PiecePriorityHigh)
	}

	var mu sync.Mutex
	emit := func(downloaded int64) {
		mu.Lock()
		defer mu.Unlock()
		if r.Context().Err() != nil {
			return
		}
		// Belt-and-suspenders against any future call path that races a
		// Flush after net/http has finalized the response — would SIGSEGV
		// in chunkWriter and take the pod down. Same pattern as
		// StatStreamServer.Send.
		defer func() {
			if rec := recover(); rec != nil {
				log.WithField("at", "warmup.emit").Warnf("recovered panic: %v", rec)
			}
		}()
		fmt.Fprintf(w, "data: %d\n\n", downloaded)
		flusher.Flush()
	}

	downloaded := warmupBytes(f.State(), rangeStart, rangeEnd)
	emit(downloaded)
	if downloaded >= total {
		return nil
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	timeout := time.NewTimer(warmupTimeout)
	defer timeout.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-t.Closed():
			return nil
		case <-timeout.C:
			log.WithFields(log.Fields{"hash": h, "path": p}).Warn("warmup timeout")
			return nil
		case <-ticker.C:
			downloaded = warmupBytes(f.State(), rangeStart, rangeEnd)
			emit(downloaded)
			if downloaded >= total {
				return nil
			}
		}
	}
}

// warmupBytes sums file bytes that are part of completed pieces and fall
// inside the requested [rangeStart, rangeEnd] file byte range.
func warmupBytes(state []torrent.FilePieceState, rangeStart, rangeEnd int64) int64 {
	var done, fileOff int64
	for _, ps := range state {
		pieceFileStart := fileOff
		pieceFileEnd := fileOff + ps.Bytes - 1
		fileOff += ps.Bytes
		if pieceFileEnd < rangeStart || pieceFileStart > rangeEnd {
			continue
		}
		if !ps.Complete {
			continue
		}
		overlapStart := pieceFileStart
		if overlapStart < rangeStart {
			overlapStart = rangeStart
		}
		overlapEnd := pieceFileEnd
		if overlapEnd > rangeEnd {
			overlapEnd = rangeEnd
		}
		done += overlapEnd - overlapStart + 1
	}
	return done
}

// parseSingleRange parses the first byte-range from an RFC 7233 Range
// header ("bytes=start-end"). Empty header → whole file. Multi-range is
// accepted but only the first range is honoured (warmup is not a real
// content transfer — multi-range has no useful meaning here).
func parseSingleRange(h string, fileLen int64) (int64, int64, error) {
	if fileLen <= 0 {
		return 0, 0, errors.Errorf("empty file")
	}
	if h == "" {
		return 0, fileLen - 1, nil
	}
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, errors.Errorf("invalid range unit")
	}
	first := h[len(prefix):]
	if i := strings.IndexByte(first, ','); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	dash := strings.IndexByte(first, '-')
	if dash < 0 {
		return 0, 0, errors.Errorf("invalid range")
	}
	startStr := strings.TrimSpace(first[:dash])
	endStr := strings.TrimSpace(first[dash+1:])

	if startStr == "" {
		// Suffix form: "-N" means last N bytes.
		if endStr == "" {
			return 0, 0, errors.Errorf("invalid range")
		}
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, errors.Errorf("invalid range")
		}
		if n > fileLen {
			n = fileLen
		}
		return fileLen - n, fileLen - 1, nil
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 || start >= fileLen {
		return 0, 0, errors.Errorf("invalid range")
	}
	if endStr == "" {
		return start, fileLen - 1, nil
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, errors.Errorf("invalid range")
	}
	if end >= fileLen {
		end = fileLen - 1
	}
	return start, end, nil
}
