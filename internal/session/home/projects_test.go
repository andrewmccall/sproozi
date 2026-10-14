//nolint:goconst // Keep independent literal behavior expectations.
package home

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/andrewmccall/sproozi/internal/checkpoint"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

func TestImportedProjectNeedsIndependentLocalApprovalAndKeepsAssociation(t *testing.T) {
	s, local, source := importedFixture(t)
	source.export.Association = project.Association{Project: "sproozi-dev", Template: "portable-codex",
		Repository: "sproozi", RepositoryReference: "https://github.com/andrewmccall/sproozi"}
	id := source.export.ID
	pulled, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote)
	if err != nil || pulled.Project != "sproozi-dev" || pulled.Repository != "sproozi" || pulled.CheckpointInfo == nil {
		t.Fatalf("Authenticated association was lost: %#v %v", pulled, err)
	}
	if _, err = s.Resume(t.Context(), id, uuid.NewString()); err == nil || local.prepares != 0 {
		t.Fatal("Imported association granted unconfigured local authority")
	}
	s.config.Allowances["kubernetes.read"] = session.Allowance{Units: session.Limit{Bounded: true, Value: 50}}
	s.config.ProjectApprovals = []ProjectApproval{{Source: "another-source", Project: "sproozi-dev",
		Capabilities: []string{"model.inference", "kubernetes.read"}}}
	if _, err = s.Resume(t.Context(), id, uuid.NewString()); err == nil || local.prepares != 0 {
		t.Fatal("A different source's approval granted execution authority")
	}
	s.config.ProjectApprovals = []ProjectApproval{{Source: testRemote, Project: "sproozi-dev",
		Capabilities: []string{"model.inference"}}}
	resumed, err := s.Resume(t.Context(), id, uuid.NewString())
	if err != nil || resumed.Project != "sproozi-dev" || resumed.ThreadID != testNativeUUID {
		t.Fatalf("Approved same-conversation continuation failed: %#v %v", resumed, err)
	}
	admitted, err := s.readInitial(resumed.ID)
	if err != nil || !reflect.DeepEqual(admitted.Launch.Allowances, map[string]session.Allowance{
		"model.inference": {Units: session.Limit{Bounded: true, Value: 10}},
	}) {
		t.Fatalf("Project widened fresh local authority: %#v %v", admitted.Launch.Allowances, err)
	}
	status, err := s.Stop(t.Context(), id, uuid.NewString())
	if err != nil || status.Project != "sproozi-dev" || status.CheckpointInfo == nil ||
		status.RepositoryReference != "https://github.com/andrewmccall/sproozi" {
		t.Fatalf("Project/checkpoint association did not survive local stop: %#v %v", status, err)
	}
	for filter, want := range map[project.ID]int{"sproozi-dev": 1, "home-ops": 0, "": 1} {
		list, listErr := s.Sessions(work.SessionQuery{Project: filter})
		if listErr != nil || len(list) != want {
			t.Fatalf("Local project filter %q returned %#v %v", filter, list, listErr)
		}
	}
}

func TestInterruptedProjectAdmissionRechecksApprovalOnEveryActivationPath(t *testing.T) {
	for _, change := range []string{"removed", "narrowed"} {
		for _, action := range []string{"resume", "recover", "move"} {
			t.Run(change+"/"+action, func(t *testing.T) {
				s, local, source := importedFixture(t)
				source.export.Project = "sproozi-dev"
				s.config.ProjectApprovals = []ProjectApproval{{Source: testRemote, Project: "sproozi-dev",
					Capabilities: []string{"model.inference"}}}
				id, operation := source.export.ID, uuid.NewString()
				if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
					t.Fatal(err)
				}
				local.failInspect = true
				if _, err := s.Resume(t.Context(), id, operation); err == nil {
					t.Fatal("Expected interrupted preparation")
				}
				local.failInspect = false
				if change == "removed" {
					s.config.ProjectApprovals = nil
				} else {
					s.config.Allowances["model.inference"] = session.Allowance{Units: session.Limit{Bounded: true, Value: 5}}
				}
				var err error
				switch action {
				case "resume":
					_, err = s.Resume(t.Context(), id, operation)
				case "recover":
					_, err = s.Recover(t.Context(), id)
				case "move":
					_, err = s.Move(t.Context(), id, uuid.NewString(), testRemote)
				}
				if err == nil || local.prepares != 0 || local.activations != 0 {
					t.Fatalf("Current approval bypassed: %v prepares=%d activations=%d", err, local.prepares, local.activations)
				}
				if _, err = s.Cancel(t.Context(), id, uuid.NewString()); err != nil {
					t.Fatalf("Revocation blocked trusted cleanup: %v", err)
				}
			})
		}
	}
}

func TestInterruptedLegacyClaimAcceptsDerivedMetadataButNotNewProject(t *testing.T) {
	for _, changedProject := range []bool{false, true} {
		t.Run(fmt.Sprint(changedProject), func(t *testing.T) {
			s, _, source := importedFixture(t)
			source.lostClaim = true
			id := source.export.ID
			if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err == nil {
				t.Fatal("Expected lost acknowledgement")
			}
			meta, err := checkpoint.Inspect(source.export.Checkpoint, testLimit)
			if err != nil {
				t.Fatal(err)
			}
			source.export.CheckpointInfo = &meta
			source.export.Repository = "sproozi"
			source.export.RepositoryReference = "https://github.com/andrewmccall/sproozi"
			if changedProject {
				source.export.Project = "sproozi-dev"
			}
			status, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote)
			if changedProject {
				if err == nil {
					t.Fatal("Claim silently changed standalone work to a Project")
				}
			} else if err != nil || status.InitialStage != "retained" || status.Repository != "sproozi" {
				t.Fatalf("Derived metadata stranded exact legacy claim: %#v %v", status, err)
			}
		})
	}
}

func TestRemovedProjectApprovalDoesNotBlockInterruptedFinalStopRecovery(t *testing.T) {
	s, _, source := importedFixture(t)
	source.export.Project = "sproozi-dev"
	s.config.ProjectApprovals = []ProjectApproval{{Source: testRemote, Project: "sproozi-dev",
		Capabilities: []string{"model.inference"}}}
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	remote := s.placements[testRemote].(*placementFixture)
	remote.lostStop = true
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("Expected lost stopped reply")
	}
	s.config.ProjectApprovals = nil
	prepared, activated := remote.prepares, remote.activations
	if _, err := s.Recover(t.Context(), id); err != nil {
		t.Fatalf("Revocation blocked stop-only recovery: %v", err)
	}
	if remote.prepares != prepared || remote.activations != activated {
		t.Fatal("Cleanup activated a new destination")
	}
}
