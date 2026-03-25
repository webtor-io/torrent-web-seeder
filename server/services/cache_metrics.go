package services

import "github.com/prometheus/client_golang/prometheus"

var (
	promCacheBytesUsed = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_cache_bytes_used",
		Help: "Current disk cache usage in bytes across all active torrents",
	})
	promCacheBudget = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_cache_budget_bytes",
		Help: "Configured per-torrent cache budget in bytes",
	})
	promCacheEvictions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_cache_evictions_total",
		Help: "Total number of piece evictions",
	})
	promCachePieceCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_cache_pieces_count",
		Help: "Current number of cached pieces across all active torrents",
	})
)

func init() {
	prometheus.MustRegister(promCacheBytesUsed)
	prometheus.MustRegister(promCacheBudget)
	prometheus.MustRegister(promCacheEvictions)
	prometheus.MustRegister(promCachePieceCount)
}
