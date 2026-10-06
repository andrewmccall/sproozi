/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package kubernetes_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/andrewmccall/sproozi/internal/kubernetes"
)

func TestRunNameDNSSafe(t *testing.T) {
	name := kubernetes.RunName("123e4567-e89b-12d3-a456-426614174000")
	// Must start with a letter (not a digit)
	if name[0] < 'a' || name[0] > 'z' {
		t.Errorf("RunName does not start with a lowercase letter: %q", name)
	}
	// Must only contain alphanumeric and hyphens
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			t.Errorf("RunName contains invalid DNS character %q: %q", c, name)
		}
	}
	// Must be at most 63 characters
	if len(name) > 63 {
		t.Errorf("RunName exceeds 63 chars: %q (len %d)", name, len(name))
	}
}

func TestRunNameExact(t *testing.T) {
	got := kubernetes.RunName("123e4567-e89b-12d3-a456-426614174000")
	want := "sproozi-123e4567e89b12d3a456426614174000"
	if got != want {
		t.Errorf("RunName = %q, want %q", got, want)
	}
}

func TestRunNameUnique(t *testing.T) {
	a := kubernetes.RunName("aaaaaaaa-0000-0000-0000-000000000000")
	b := kubernetes.RunName("bbbbbbbb-0000-0000-0000-000000000000")
	if a == b {
		t.Errorf("RunName produced the same name for different UIDs: %q", a)
	}
}

func TestRunNameStripsHyphens(t *testing.T) {
	uid := types.UID("123e4567-e89b-12d3-a456-426614174000")
	name := kubernetes.RunName(uid)
	// Must not contain the raw UID with hyphens
	if strings.Contains(name, string(uid)) {
		t.Errorf("RunName leaks raw UID: %q", name)
	}
}
