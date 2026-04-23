package main

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	s "github.com/webtor-io/torrent-web-seeder/server/services"
)

const (
	DiagnoseTimeoutFlag = "timeout"
)

func configureDiagnose(app *cli.App) {
	diagnoseFlags := []cli.Flag{
		cli.DurationFlag{
			Name:  DiagnoseTimeoutFlag,
			Usage: "diagnostic timeout",
			Value: 60 * time.Second,
		},
	}
	diagnoseFlags = s.RegisterTorrentClientFlags(diagnoseFlags)

	app.Commands = append(app.Commands, cli.Command{
		Name:      "diagnose",
		Aliases:   []string{"diag"},
		Usage:     "Diagnose torrent download issues",
		ArgsUsage: "<magnet-uri or path to .torrent file>",
		Flags:     diagnoseFlags,
		Action:    runDiagnose,
	})
}

func runDiagnose(c *cli.Context) error {
	input := c.Args().First()
	if input == "" {
		return fmt.Errorf("usage: torrent-web-seeder diagnose <magnet-uri or .torrent path>")
	}

	// Suppress logrus Info messages to keep diagnostic output clean
	prevLevel := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	defer log.SetLevel(prevLevel)

	timeout := c.Duration(DiagnoseTimeoutFlag)

	fmt.Println("=== Torrent Diagnostics ===")
	fmt.Println()

	// Phase 1: Client Init
	fmt.Println("--- Client Initialization ---")
	torrentClient, err := s.NewTorrentClient(c)
	if err != nil {
		fmt.Printf("[FAIL] Client init error: %v\n", err)
		return err
	}
	defer torrentClient.Close()

	cl, err := torrentClient.Get()
	if err != nil {
		fmt.Printf("[FAIL] Client start error: %v\n", err)
		return err
	}
	fmt.Println("[OK]   Client started")

	addrs := cl.ListenAddrs()
	if len(addrs) > 0 {
		addrStrs := make([]string, len(addrs))
		for i, a := range addrs {
			addrStrs[i] = a.String()
		}
		fmt.Printf("       Listen: %s\n", strings.Join(addrStrs, ", "))
	}

	dhtServers := cl.DhtServers()
	fmt.Printf("       DHT: %d server(s)\n", len(dhtServers))
	fmt.Println()

	// Phase 2: Add Torrent
	fmt.Println("--- Torrent Input ---")
	var t *torrent.Torrent

	isMagnet := strings.HasPrefix(input, "magnet:")
	if isMagnet {
		m, err := metainfo.ParseMagnetUri(input)
		if err != nil {
			fmt.Printf("[FAIL] Invalid magnet URI: %v\n", err)
			return err
		}
		fmt.Printf("       Type: magnet link\n")
		fmt.Printf("       Info Hash: %s\n", m.InfoHash.HexString())
		if m.DisplayName != "" {
			fmt.Printf("       Display Name: %s\n", m.DisplayName)
		}
		fmt.Printf("       Trackers: %d\n", len(m.Trackers))
		for _, tr := range m.Trackers {
			fmt.Printf("         - %s\n", tr)
		}

		t, err = cl.AddMagnet(input)
		if err != nil {
			fmt.Printf("[FAIL] Add magnet: %v\n", err)
			return err
		}
		fmt.Println("[OK]   Magnet added to client")
	} else {
		mi, err := metainfo.LoadFromFile(input)
		if err != nil {
			fmt.Printf("[FAIL] Load .torrent file: %v\n", err)
			return err
		}
		info, err := mi.UnmarshalInfo()
		if err != nil {
			fmt.Printf("[FAIL] Parse torrent info: %v\n", err)
			return err
		}
		fmt.Printf("       Type: .torrent file\n")
		fmt.Printf("       Info Hash: %s\n", mi.HashInfoBytes().HexString())
		fmt.Printf("       Name: %s\n", info.Name)
		fmt.Printf("       Size: %s\n", formatBytes(info.TotalLength()))

		trackers := mi.UpvertedAnnounceList()
		trackerCount := 0
		for _, tier := range trackers {
			trackerCount += len(tier)
		}
		fmt.Printf("       Trackers: %d\n", trackerCount)
		for _, tier := range trackers {
			for _, tr := range tier {
				fmt.Printf("         - %s\n", tr)
			}
		}

		t, err = cl.AddTorrent(mi)
		if err != nil {
			fmt.Printf("[FAIL] Add torrent: %v\n", err)
			return err
		}
		fmt.Println("[OK]   Torrent added to client")
	}
	fmt.Println()

	// Phase 3: Metadata Resolution
	fmt.Println("--- Metadata Resolution ---")
	metaStart := time.Now()
	gotInfo := false

	if t.Info() != nil {
		gotInfo = true
		fmt.Println("[OK]   Metadata already available")
	} else {
		fmt.Printf("[..]   Waiting for metadata (timeout: %v)...\n", timeout)
		metaTimer := time.NewTimer(timeout)
		defer metaTimer.Stop()

		metaTicker := time.NewTicker(5 * time.Second)
		defer metaTicker.Stop()

	metaLoop:
		for {
			select {
			case <-t.GotInfo():
				gotInfo = true
				fmt.Printf("[OK]   Metadata received in %.1fs\n", time.Since(metaStart).Seconds())
				break metaLoop
			case <-metaTicker.C:
				stats := t.Stats()
				fmt.Printf("[..]   %5.0fs elapsed  peers=%d seeders=%d half-open=%d\n",
					time.Since(metaStart).Seconds(),
					stats.ActivePeers,
					stats.ConnectedSeeders,
					stats.HalfOpenPeers,
				)
			case <-metaTimer.C:
				fmt.Printf("[FAIL] Metadata not received within %v\n", timeout)
				break metaLoop
			}
		}
	}

	if gotInfo {
		info := t.Info()
		fmt.Printf("       Name: %s\n", info.Name)
		fmt.Printf("       Size: %s\n", formatBytes(info.TotalLength()))
		fmt.Printf("       Pieces: %d x %s\n", info.NumPieces(), formatBytes(info.PieceLength))
		files := info.UpvertedFiles()
		fmt.Printf("       Files: %d\n", len(files))
		for i, f := range files {
			if i >= 10 {
				fmt.Printf("         ... and %d more\n", len(files)-10)
				break
			}
			fmt.Printf("         %s (%s)\n", strings.Join(f.BestPath(), "/"), formatBytes(f.Length))
		}
	}
	fmt.Println()

	// Phase 4: Download Test (only if metadata available)
	if gotInfo {
		fmt.Println("--- Download Test ---")
		t.DownloadAll()

		dlDuration := 30 * time.Second
		remaining := timeout - time.Since(metaStart)
		if remaining < dlDuration {
			dlDuration = remaining
		}
		if dlDuration < 5*time.Second {
			dlDuration = 5 * time.Second
		}

		fmt.Printf("[..]   Testing download for %v...\n", dlDuration.Round(time.Second))

		dlTimer := time.NewTimer(dlDuration)
		defer dlTimer.Stop()

		dlTicker := time.NewTicker(3 * time.Second)
		defer dlTicker.Stop()

		dlStart := time.Now()
		firstByteTime := time.Duration(0)
		prevBytes := int64(0)

	dlLoop:
		for {
			select {
			case <-dlTimer.C:
				break dlLoop
			case <-dlTicker.C:
				stats := t.Stats()
				bytesRead := stats.BytesReadUsefulData.Int64()

				if firstByteTime == 0 && bytesRead > 0 {
					firstByteTime = time.Since(dlStart)
				}

				speed := float64(bytesRead-prevBytes) / 3.0
				prevBytes = bytesRead
				elapsed := time.Since(dlStart).Seconds()

				fmt.Printf("       %5.0fs  peers=%-3d seeders=%-3d downloaded=%-12s speed=%s/s\n",
					elapsed,
					stats.ActivePeers,
					stats.ConnectedSeeders,
					formatBytes(bytesRead),
					formatBytes(int64(speed)),
				)
			}
		}

		if firstByteTime > 0 {
			fmt.Printf("[OK]   First useful data at %.1fs\n", firstByteTime.Seconds())
		} else {
			fmt.Println("[WARN] No useful data received during test")
		}
		fmt.Println()
	}

	// Phase 5: Capture full status dump
	var statusBuf bytes.Buffer
	cl.WriteStatus(&statusBuf)
	statusOutput := statusBuf.String()

	// Phase 6: Tracker Diagnostics (extracted from status dump)
	trackerLines := extractTrackerLines(statusOutput)
	if len(trackerLines) > 0 {
		fmt.Println("--- Tracker Status ---")
		for _, line := range trackerLines {
			marker := "[OK]  "
			if strings.Contains(line.status, "failure reason") ||
				strings.Contains(line.status, "error") ||
				strings.Contains(line.status, "refused") ||
				strings.Contains(line.status, "timeout") {
				marker = "[FAIL]"
			} else if strings.Contains(line.status, "announcing") {
				marker = "[..]  "
			}
			fmt.Printf("%s %s\n", marker, line.url)
			fmt.Printf("       %s\n", line.status)
		}
		fmt.Println()
	}

	// Phase 7: Final Statistics
	fmt.Println("--- Final Statistics ---")
	stats := t.Stats()
	fmt.Printf("Total Peers:       %d\n", stats.TotalPeers)
	fmt.Printf("Active Peers:      %d\n", stats.ActivePeers)
	fmt.Printf("Connected Seeders: %d\n", stats.ConnectedSeeders)
	fmt.Printf("Half-Open:         %d\n", stats.HalfOpenPeers)
	fmt.Printf("Pending Peers:     %d\n", stats.PendingPeers)
	fmt.Printf("Bytes Read:        %s (total) / %s (useful data)\n",
		formatBytes(stats.BytesRead.Int64()),
		formatBytes(stats.BytesReadUsefulData.Int64()))
	if gotInfo {
		fmt.Printf("Pieces Complete:   %d / %d\n", stats.PiecesComplete, t.NumPieces())
	}
	fmt.Println()

	// Phase 8: Connected Peers Detail
	peerConns := t.PeerConns()
	if len(peerConns) > 0 {
		fmt.Printf("--- Connected Peers (%d) ---\n", len(peerConns))
		for i, pc := range peerConns {
			if i >= 20 {
				fmt.Printf("  ... and %d more\n", len(peerConns)-20)
				break
			}
			ps := pc.Stats()
			fmt.Printf("  %-45s down=%-8s/s data=%s\n",
				pc.RemoteAddr,
				formatBytes(int64(ps.DownloadRate)),
				formatBytes(ps.BytesReadUsefulData.Int64()),
			)
		}
		fmt.Println()
	}

	// Phase 9: Full Status Dump
	fmt.Println("--- Raw Client Status ---")
	fmt.Print(statusOutput)
	fmt.Println()

	// Phase 10: Diagnosis Summary
	fmt.Println("=== Diagnosis ===")
	printDiagnosis(gotInfo, stats, trackerLines)
	fmt.Println()

	return nil
}

type trackerLine struct {
	url    string
	status string
}

// extractTrackerLines parses the WriteStatus output to extract per-tracker status lines.
// Format: `    "https://tracker.example.com/announce"  next ann: 45s, last ann: ...`
func extractTrackerLines(statusOutput string) []trackerLine {
	var results []trackerLine
	lines := strings.Split(statusOutput, "\n")
	inTrackers := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Enabled trackers:" {
			inTrackers = true
			continue
		}
		if inTrackers {
			if trimmed == "" || (!strings.HasPrefix(trimmed, "\"") && !strings.HasPrefix(trimmed, "URL")) {
				inTrackers = false
				continue
			}
			if strings.HasPrefix(trimmed, "URL") {
				continue
			}
			// Parse: "URL"  status...
			if idx := strings.Index(trimmed[1:], "\""); idx >= 0 {
				url := trimmed[1 : idx+1]
				status := strings.TrimSpace(trimmed[idx+2:])
				results = append(results, trackerLine{url: url, status: status})
			}
		}
	}
	return results
}

func printDiagnosis(gotInfo bool, stats torrent.TorrentStats, trackers []trackerLine) {
	// Check for tracker failures
	trackerFailures := 0
	for _, t := range trackers {
		if strings.Contains(t.status, "failure reason") ||
			strings.Contains(t.status, "error") ||
			strings.Contains(t.status, "refused") ||
			strings.Contains(t.status, "timeout") {
			trackerFailures++
		}
	}
	allTrackersFailed := len(trackers) > 0 && trackerFailures == len(trackers)

	if !gotInfo {
		fmt.Println("[PROBLEM] Could not resolve torrent metadata.")
		if allTrackersFailed {
			fmt.Println("  All trackers failed! Check tracker errors above.")
			fmt.Println("  Common causes:")
			fmt.Println("  - Tracker blocked your IP address")
			fmt.Println("  - Torrent was removed from tracker")
			fmt.Println("  - Tracker is down or unreachable")
		} else {
			fmt.Println("  Possible causes:")
			fmt.Println("  - No peers/seeders available for this torrent")
			fmt.Println("  - All trackers are unreachable or blocked your IP")
			fmt.Println("  - DHT is not finding peers for this info hash")
			fmt.Println("  - Firewall blocking BitTorrent traffic (TCP/UDP)")
		}
		fmt.Println("  Suggestions:")
		fmt.Println("  - Try with --torrent-client-debug for verbose output")
		fmt.Println("  - Check if trackers are accessible from this network")
		fmt.Println("  - Try with --http-proxy to test from a different IP")
		return
	}

	if stats.TotalPeers == 0 {
		fmt.Println("[PROBLEM] No peers discovered.")
		if allTrackersFailed {
			fmt.Println("  All trackers failed — see tracker errors above.")
		}
		fmt.Println("  Possible causes:")
		fmt.Println("  - Torrent is dead (no active seeders/leechers)")
		fmt.Println("  - Trackers are blocking your IP")
		fmt.Println("  - DHT/PEX not discovering peers")
		fmt.Println("  Suggestions:")
		fmt.Println("  - Try the same magnet in a regular torrent client for comparison")
		return
	}

	if stats.ConnectedSeeders == 0 && stats.ActivePeers > 0 {
		fmt.Println("[WARNING] Peers found but no seeders connected.")
		fmt.Println("  - Only leechers are available — torrent may be partially seeded")
		fmt.Println("  - Try waiting longer or check if seeders are available at different times")
		return
	}

	if stats.BytesReadUsefulData.Int64() == 0 && stats.ConnectedSeeders > 0 {
		fmt.Println("[WARNING] Seeders connected but no data received.")
		fmt.Println("  Possible causes:")
		fmt.Println("  - Seeders are choking this client")
		fmt.Println("  - Download rate limit too restrictive (check --download-rate)")
		fmt.Println("  - Piece verification issues")
		return
	}

	fmt.Println("[OK] Torrent appears healthy.")
	fmt.Printf("     Peers: %d active, %d seeders\n", stats.ActivePeers, stats.ConnectedSeeders)
	fmt.Printf("     Downloaded: %s useful data\n", formatBytes(stats.BytesReadUsefulData.Int64()))
	if trackerFailures > 0 {
		fmt.Printf("     Note: %d/%d trackers failed — see tracker status above\n", trackerFailures, len(trackers))
	}
}

func formatBytes(b int64) string {
	if b < 0 {
		return "0 B"
	}
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
