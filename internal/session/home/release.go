package home

import (
	"context"
	"fmt"
	"time"
)

// releaseInterval is how often the service finishes removing the containers
// and volumes of visits whose stop is durable.
const releaseInterval = 5 * time.Minute

// stoppedReleaser is implemented by placements that retain a removal for a
// later call when it fails after a durable stop.
type stoppedReleaser interface {
	ReleaseStopped(ctx context.Context) (int, error)
}

// releaseStopped owns the retry of removals that Stop and Abort leave behind:
// once at start, then every releaseInterval until ctx ends.
func (s *Service) releaseStopped(ctx context.Context) {
	ticker := time.NewTicker(releaseInterval)
	defer ticker.Stop()
	for {
		s.releaseStoppedOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) releaseStoppedOnce(ctx context.Context) {
	logf := func(format string, args ...any) {
		if s.log != nil {
			_, _ = fmt.Fprintf(s.log, format, args...)
		}
	}
	for name, placement := range s.placementsSnapshot() {
		releaser, ok := placement.(stoppedReleaser)
		if !ok {
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, releaseInterval)
		released, err := releaser.ReleaseStopped(attempt)
		cancel()
		if err != nil {
			logf("Failed to remove stopped visit resources for placement %s: %v\n", name, err)
		}
		if released > 0 {
			logf("Removed resources of %d stopped visits for placement %s\n", released, name)
		}
	}
}
