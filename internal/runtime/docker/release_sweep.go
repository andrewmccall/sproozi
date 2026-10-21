package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"time"
)

// releaseVisitTimeout bounds the engine calls for one visit, so a hung Docker
// call holds that visit's operation lock, and delays a concurrent Stop, for at
// most this long.
const releaseVisitTimeout = time.Minute

// recordFile matches the journal name save writes for one visit.
var recordFile = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)

// ReleaseStopped finishes removal for every durably stopped or aborted visit
// whose containers or volume are still recorded as present. Stop and Abort
// return success once the stop is durable and leave a failed removal for a
// later call, which a coordinator has no reason to make. The local service
// calls this at start and then periodically, so it owns that later call.
// It returns how many visits it released and the first error.
func (p *Placement) ReleaseStopped(ctx context.Context) (int, error) {
	p.mu.RLock()
	if err := p.ready(); err != nil {
		p.mu.RUnlock()
		return 0, err
	}
	entries, err := os.ReadDir(p.dir)
	p.mu.RUnlock()
	if err != nil {
		return 0, err
	}
	released := 0
	var first error
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !recordFile.MatchString(entry.Name()) {
			continue
		}
		done, err := p.releaseRecord(ctx, strings.TrimSuffix(entry.Name(), ".json"))
		if done {
			released++
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return released, first
}

func (p *Placement) releaseRecord(ctx context.Context, key string) (bool, error) {
	data, err := p.read(key+".json", maxRecordBytes)
	if err != nil {
		return false, err
	}
	var peek placementRecord
	if err := json.Unmarshal(data, &peek); err != nil {
		return false, err
	}
	if operationKey(peek.Launch) != key {
		return false, errors.New("docker placement record name does not match its launch")
	}
	// Skip a visit that is plainly active or already removed without taking
	// its lock, and skip one whose lock is held: an active operation must not
	// delay cleanup of other visits. Both are rechecked on the next sweep.
	if peek.Removed || !peek.releasable() {
		return false, nil
	}
	unlock, ok, err := p.tryLockOperation(peek.Launch)
	if err != nil || !ok {
		return false, err
	}
	defer unlock()
	// load re-reads under the operation lock and checks the record's intent.
	r, err := p.load(peek.Launch)
	if err != nil {
		return false, err
	}
	if r.Removed || !r.releasable() {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, releaseVisitTimeout)
	defer cancel()
	if err := p.release(ctx, &r); err != nil {
		return false, err
	}
	return true, nil
}
