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

package main

import (
	"net/http"
	"testing"
)

// TestNewServerBoundsEveryConnectionPhase verifies that the receiver, which
// accepts connections before authentication, cannot be held open by a slow
// body, a slow reader, an idle connection or oversized headers.
func TestNewServerBoundsEveryConnectionPhase(t *testing.T) {
	server := newServer(":0", http.NotFoundHandler())
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 ||
		server.IdleTimeout <= 0 || server.MaxHeaderBytes <= 0 || server.MaxHeaderBytes > maxHeaderBytes {
		t.Fatalf("server limits are incomplete: header=%s read=%s write=%s idle=%s maxHeader=%d",
			server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout, server.IdleTimeout, server.MaxHeaderBytes)
	}
}
