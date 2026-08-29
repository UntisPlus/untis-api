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

// handleStatus reports the process status: uptime, mode (dev/beta/prod) and
// version. It is self-measured only; external uptime monitoring is out of scope.
func (p *Proxy) handleStatus(w http.ResponseWriter, r *http.Request) {
	mode := os.Getenv("UNTIS_ENV")
	if mode == "" {
		mode = "dev"
	}
	v := os.Getenv("UNTIS_VERSION")
	if v == "" {
		v = version
	}
	p.writeJSON(w, map[string]any{
		"status":     "ok",
		"mode":       mode,
		"version":    v,
		"uptime_sec": int64(time.Since(startedAt).Seconds()),
	})
}
