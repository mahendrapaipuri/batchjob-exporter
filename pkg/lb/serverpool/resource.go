package serverpool

import (
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/mahendrapaipuri/ceems/pkg/lb/backend"
)

// resourceBased implements resource based load balancer strategy. The resource is
// the retention period of each TSDB.
//
// Based on the request's "from" timestamp and backend TSDB retention period, load
// balancer will make a decision on which backend TSDB server to use. If a request
// can be served by multiple backend TSDB servers, the one with least retention period
// will be chosen as it is assumed as "hot" TSDB with maximum performance.
type resourceBased struct {
	backends map[string][]backend.Server
	logger   *slog.Logger
}

// Target returns the backend server to send the request if it is alive.
func (s *resourceBased) Target(id string, d time.Duration) backend.Server {
	// If the ID is unknown return
	if _, ok := s.backends[id]; !ok {
		s.logger.Error("Resource based strategy", "err", fmt.Errorf("unknown backend ID: %s", id))

		return nil
	}

	// Get a list of eligible TSDB servers based on retention period and
	// start time of TSDB query
	var targetBackend backend.Server

	var targetBackends []backend.Server

	var retentionPeriods []time.Duration

	for i := range s.Size(id) {
		if !s.backends[id][i].IsAlive() {
			continue
		}

		// If query duration is less than backend TSDB's retention period, it is
		// target backend as it can serve the query
		if d < s.backends[id][i].RetentionPeriod() {
			targetBackends = append(targetBackends, s.backends[id][i])
			retentionPeriods = append(retentionPeriods, s.backends[id][i].RetentionPeriod())
		}
	}

	// If no eligible servers found return
	if len(targetBackends) == 0 {
		s.logger.Debug("Resourced based strategy. No eligible backends found", "cluster_id", id, "duration", d)

		return targetBackend
	} else if len(targetBackends) == 1 {
		targetBackend = targetBackends[0]
		s.logger.Debug("Resourced based strategy", "cluster_id", id, "selected_backend", targetBackend.String())

		return targetBackend
	}

	// Get the minimum retention period from all eligible backends
	minRetentionPeriod := slices.Min(retentionPeriods)

	// If multiple eligible servers has same retention period as minimum retention
	// period, return the one that has least connections
	activeConnections := math.MaxInt32

	for i := range targetBackends {
		if !targetBackends[i].IsAlive() {
			continue
		}

		if retentionPeriods[i] == minRetentionPeriod {
			backendActiveConnections := targetBackends[i].ActiveConnections()
			if activeConnections > backendActiveConnections {
				targetBackend = targetBackends[i]
				activeConnections = backendActiveConnections
			}
		}
	}

	if targetBackend != nil {
		s.logger.Debug("Resourced based strategy", "cluster_id", id, "selected_backend", targetBackend.String())

		return targetBackend
	}

	return nil
}

// List all backend servers in pool.
func (s *resourceBased) Backends() map[string][]backend.Server {
	return s.backends
}

// Add a backend server to pool.
func (s *resourceBased) Add(id string, b backend.Server) {
	s.logger.Debug("Backend added", "strategy", "resource", "cluster_id", id, "backend", b.String())

	s.backends[id] = append(s.backends[id], b)
}

// Total number of backend servers in pool.
func (s *resourceBased) Size(id string) int {
	return len(s.backends[id])
}
