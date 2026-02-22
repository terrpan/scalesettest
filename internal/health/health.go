// Package health provides HTTP handlers for health checks.
package health

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/terrpan/scaleset/internal/buildinfo"
)

// Response represents the health check response body.
type Response struct {
	Status       string    `json:"status"`
	ServiceName  string    `json:"service_name"`
	Version      string    `json:"version"`
	Commit       string    `json:"commit"`
	BuildTime    string    `json:"build_time"`
	GoVersion    string    `json:"go_version"`
	OS           string    `json:"os"`
	Architecture string    `json:"architecture"`
	Engine       string    `json:"engine"`
	Timestamp    time.Time `json:"timestamp"`
}

// Handler responds to health check requests. It reports build info and the
// enabled compute engine. The status is always "healthy" (200 OK) since this
// is a liveness check with no external dependencies to verify.
func Handler(engine string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		response := Response{
			Status:       "healthy",
			ServiceName:  "scaleset",
			Version:      buildinfo.Version,
			Commit:       buildinfo.Commit,
			BuildTime:    buildinfo.BuildTime,
			GoVersion:    runtime.Version(),
			OS:           runtime.GOOS,
			Architecture: runtime.GOARCH,
			Engine:       engine,
			Timestamp:    time.Now().UTC(),
		}

		_ = json.NewEncoder(w).Encode(response)
	}
}
