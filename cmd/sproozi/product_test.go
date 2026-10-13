package main

import (
	"reflect"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

const sharedOwner = "shared"

func TestGlobalSessionDiscoveryIncludesSeparateWorkOwners(t *testing.T) {
	c := home.Config{
		Remote:      map[string]profile.ClientConfig{"engineering": {}, sharedOwner: {}},
		WorkSources: map[string]profile.ClientConfig{"tasks": {}, sharedOwner: {}},
	}
	if got := selectedExecutors(c, ""); !reflect.DeepEqual(got, []string{"engineering", sharedOwner, "tasks"}) {
		t.Fatalf("configured owners=%v", got)
	}
	if got := selectedExecutors(c, dockerExecutor); len(got) != 0 {
		t.Fatalf("explicit Docker selection queried remote owners: %v", got)
	}
}
