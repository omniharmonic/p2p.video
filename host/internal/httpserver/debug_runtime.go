package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
)

// RuntimeStats is the loopback-only process snapshot used by soak tests and
// local diagnosis [SL-P0-6]: goroutines, Go heap, open descriptors and
// signaling/SFU object counts. It carries no session, participant or token
// data.
type RuntimeStats struct {
	Goroutines     int            `json:"goroutines"`
	HeapAllocBytes uint64         `json:"heapAllocBytes"`
	HeapObjects    uint64         `json:"heapObjects"`
	SysBytes       uint64         `json:"sysBytes"`
	NumGC          uint32         `json:"numGC"`
	OpenFDs        int            `json:"openFDs"` // -1 when the platform has no /dev/fd
	Counts         map[string]int `json:"counts,omitempty"`
}

// handleDebugRuntime serves RuntimeStats only to a direct loopback request:
// never through Tailscale Serve or Funnel (both arrive via a loopback proxy
// with forwarding headers), and never to a browser page from another origin.
func (s *Server) handleDebugRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !localManagement(r) || r.Header.Get("Tailscale-Funnel-Request") != "" || r.Header.Get("Tailscale-User-Login") != "" || r.Header.Get("Origin") != "" {
		http.Error(w, "local diagnostics only", http.StatusForbidden)
		return
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	stats := RuntimeStats{
		Goroutines:     runtime.NumGoroutine(),
		HeapAllocBytes: m.HeapAlloc,
		HeapObjects:    m.HeapObjects,
		SysBytes:       m.Sys,
		NumGC:          m.NumGC,
		OpenFDs:        countOpenFDs(),
	}
	if s.RuntimeCounts != nil {
		stats.Counts = s.RuntimeCounts()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(stats)
}

// countOpenFDs lists /dev/fd by name only. os.ReadDir would lstat every
// entry (macOS's fdesc reports no entry type), and a descriptor closed in
// between by another goroutine failed the whole listing, so a busy daemon
// reported -1 [SL-P5-4].
func countOpenFDs() int {
	dir, err := os.Open("/dev/fd")
	if err != nil {
		return -1
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return -1
	}
	// The listing itself holds one descriptor open on /dev/fd.
	return max(0, len(names)-1)
}
