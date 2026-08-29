package proxy

import (
	"net/http"
	"os"
	"time"
)

var startedAt = time.Now()

// version is the reported build version; set at build time via
// -ldflags "-X untis-proxy/internal/proxy.version=<...>". Defaults to "dev".
var version = "dev"

// handleStatus reports the process status: mode (dev/beta/prod), version,
// uptime and a snapshot of the pool. It is self-measured only; external
// uptime monitoring is out of scope here.
func (p *Proxy) handleStatus(w http.ResponseWriter, r *http.Request) {
	mode := os.Getenv("UNTIS_ENV")
	if mode == "" {
		mode = "dev"
	}
	v := os.Getenv("UNTIS_VERSION")
	if v == "" {
		v = version
	}
	cls, _ := p.store.Pool()
	pool := make([]struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}, 0, len(cls))
	for _, c := range cls {
		pool = append(pool, struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}{c.ID, c.Name})
	}
	users, _ := p.store.UserCount()
	elems := p.recon.counts()
	p.writeJSON(w, map[string]any{
		"status":     "ok",
		"mode":       mode,
		"version":    v,
		"uptime_sec": int64(time.Since(startedAt).Seconds()),
		"started_at": startedAt.UTC().Format(time.RFC3339),
		"school":     p.opts.School,
		"pool": map[string]any{
			"classes":  len(pool),
			"users":    users,
			"elements": map[string]int{"teachers": elems["TEACHER"], "rooms": elems["ROOM"], "subjects": elems["SUBJECT"]},
			"class":    pool,
		},
	})
}
