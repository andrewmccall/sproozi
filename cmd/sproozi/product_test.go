package main

import (
	"reflect"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

const sharedOwner = "shared"
const testExecutor = "engineering"
const testSocketFlag = "--socket"
const testHomeOwnership = "home"
const testAvailable = "available"
const testUnavailable = "unavailable"

func TestGlobalSessionDiscoveryIncludesSeparateWorkOwners(t *testing.T) {
	c := home.Config{
		Remote:      map[string]profile.ClientConfig{testExecutor: {}, sharedOwner: {}, dockerExecutor: {}},
		WorkSources: map[string]profile.ClientConfig{"tasks": {}, sharedOwner: {}, dockerExecutor: {}},
	}
	if got := selectedExecutors(c, ""); !reflect.DeepEqual(got, []string{testExecutor, sharedOwner, "tasks"}) {
		t.Fatalf("configured owners=%v", got)
	}
	if got := selectedExecutors(c, dockerExecutor); len(got) != 0 {
		t.Fatalf("explicit Docker selection queried remote owners: %v", got)
	}
}
