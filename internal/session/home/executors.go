package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/remote"
	"github.com/andrewmccall/sproozi/internal/work"
)

// executorsFile records the remote executors this service has reached
// through kubeconfig contexts, so it reaches them again after the CLI exits
// and after a restart.
const executorsFile = "executors.json"

type executorRecords struct {
	Version   int                        `json:"version"`
	Executors map[string]remote.Executor `json:"executors"`
}

// openExecutors builds a client for every recorded executor. It contacts no
// cluster, so the service starts while a cluster is unreachable.
func (s *Service) openExecutors() error {
	s.remotes = map[string]*remote.Client{}
	s.workSources = map[string]WorkSource{}
	data, err := readPrivate(filepath.Join(s.config.Directory, executorsFile), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var records executorRecords
	if err = decode(data, &records); err != nil || records.Version != 1 {
		return fmt.Errorf("home: %s is not a valid record of remote executors; move it aside to start without them",
			filepath.Join(s.config.Directory, executorsFile))
	}
	for _, name := range slices.Sorted(maps.Keys(records.Executors)) {
		e := records.Executors[name]
		if e.Name != name {
			return fmt.Errorf("home: %s records %q under the name %q", executorsFile, e.Name, name)
		}
		if err = s.addExecutor(e); err != nil {
			return err
		}
	}
	return nil
}

// addExecutor registers a client for a newly recorded executor as a
// placement and a work source.
func (s *Service) addExecutor(e remote.Executor) error {
	if problem := s.executorNameProblem(e.Name); problem != "" {
		return errors.New("home: " + problem)
	}
	client, err := remote.NewClient(e)
	if err != nil {
		return err
	}
	if s.remotes == nil {
		s.remotes = map[string]*remote.Client{}
	}
	if s.workSources == nil {
		s.workSources = map[string]WorkSource{}
	}
	if s.coordinator != nil {
		if err = s.coordinator.AddPlacement(e.Name, client); err != nil {
			return err
		}
		if s.config.Local.ProfileDirectory != "" {
			client.SetProfileSource(s.coordinator.ProfileArchive)
		}
	}
	s.remotes[e.Name] = client
	s.placements[e.Name] = client
	s.workSources[e.Name] = work.ClientFor(client)
	return nil
}

// executorNameProblem refuses an executor name that would shadow this machine.
func (s *Service) executorNameProblem(name string) string {
	if name == LocalAlias || s.localPlacementName(name) {
		return fmt.Sprintf("the remote executor is named %q, which is this machine's own name; "+
			"ask the administrator to give it another placementName", name)
	}
	if problem := ExecutorNameProblem(name); problem != "" {
		return "the remote executor's name " + problem + "; ask the administrator to change its placementName"
	}
	return ""
}

func (s *Service) localPlacementName(name string) bool {
	for _, stack := range s.stacks {
		if stack.config.Name == name {
			return true
		}
	}
	return name == s.config.Local.Name
}

// reachExecutor finds the executor a context selects, from this service's own
// environment, so exec credential plugins run here. It checks the checkpoint
// limit, records the executor and returns its name. A known name is pointed at
// the new selection: the executor's name, not the context, identifies it.
func (s *Service) reachExecutor(ctx context.Context, sel remote.Selection) (string, error) {
	e, err := s.discover(ctx, sel)
	if err != nil {
		return "", rejectedError(err.Error())
	}
	if err = e.CheckpointProblem(s.config.Local.MaxCheckpointBytes); err != nil {
		return "", rejectedError(err.Error())
	}
	if problem := s.executorNameProblem(e.Name); problem != "" {
		return "", rejectedError(problem)
	}
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	if existing := s.remotes[e.Name]; existing != nil {
		if err = existing.Update(e); err != nil {
			return "", err
		}
	} else if err = s.addExecutor(e); err != nil {
		return "", err
	}
	return e.Name, s.saveExecutors()
}

// saveExecutors writes every recorded executor; remoteMu is held.
func (s *Service) saveExecutors() error {
	records := executorRecords{Version: 1, Executors: map[string]remote.Executor{}}
	for name, client := range s.remotes {
		records.Executors[name] = client.Executor()
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.config.Directory, executorsFile, data)
}

// workSource finds a recorded executor's remote work catalogue.
func (s *Service) workSource(name string) WorkSource {
	s.remoteMu.RLock()
	defer s.remoteMu.RUnlock()
	return s.workSources[name]
}

// placementsSnapshot copies the placements under remoteMu, so a caller can
// call into each without holding the lock while executor discovery adds one.
func (s *Service) placementsSnapshot() map[string]session.Placement {
	s.remoteMu.RLock()
	defer s.remoteMu.RUnlock()
	return maps.Clone(s.placements)
}

// placement finds a destination by name: a local placement or a recorded executor.
func (s *Service) placement(name string) session.Placement {
	s.remoteMu.RLock()
	defer s.remoteMu.RUnlock()
	return s.placements[name]
}

// pullRetained reports a pull this service already began, which continues
// with its recorded source whatever context the retry names.
func (s *Service) pullRetained(id string) bool {
	_, err := s.readCatalog(id)
	return err == nil
}
