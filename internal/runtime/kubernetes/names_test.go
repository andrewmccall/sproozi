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
	"testing"

	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

func TestRunNameExact(t *testing.T) {
	got := kubernetes.RunName("123e4567-e89b-12d3-a456-426614174000")
	want := "sproozi-123e4567e89b12d3a456426614174000"
	if got != want {
		t.Errorf("RunName = %q, want %q", got, want)
	}
}
