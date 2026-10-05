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

package github

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

const (
	foreignBranchDenial = "not owned by this run"
)

const testRunUID = "run-abc-uid"

func makePktLine(s string) []byte { return []byte(fmt.Sprintf("%04x%s", len(s)+4, s)) }
func makeFlush() []byte           { return []byte("0000") }

func makePush(oldOID, newOID, ref string) []byte {
	command := fmt.Sprintf("%s %s %s\x00report-status side-band-64k", oldOID, newOID, ref)
	body := append(makePktLine(command), makeFlush()...)
	return append(body, []byte("PACKopaque")...)
}

type fragmentReader struct{ data []byte }

func (r *fragmentReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestInspectReceivePackAllowsFragmentedRunOwnedBranchCreationAndRestoresBody(t *testing.T) {
	body := makePush(strings.Repeat("0", 40), strings.Repeat("a", 40), "refs/heads/sproozi/run-abc-uid/fix-image")
	original := append([]byte(nil), body...)

	restored, err := inspectReceivePack(io.NopCloser(&fragmentReader{data: body}), testRunUID)
	if err != nil {
		t.Fatalf("inspectReceivePack() error = %v", err)
	}
	got, err := io.ReadAll(restored)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("inspection did not restore the exact receive-pack request")
	}
}

func TestInspectReceivePackRejectsOutOfScopeRefCommands(t *testing.T) {
	zero := strings.Repeat("0", 40)
	old := strings.Repeat("b", 40)
	newOID := strings.Repeat("a", 40)
	runRef := "refs/heads/sproozi/run-abc-uid/fix-image"

	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "delete", body: makePush(old, zero, runRef), want: "deletion is not permitted"},
		{name: "update or force", body: makePush(old, newOID, runRef), want: "ref updates are not permitted"},
		{name: "tag", body: makePush(zero, newOID, "refs/tags/sproozi/run-abc-uid/v1"), want: foreignBranchDenial},
		{name: "other run", body: makePush(zero, newOID, "refs/heads/sproozi/other-run/fix"), want: foreignBranchDenial},
		{name: "unscoped branch", body: makePush(zero, newOID, "refs/heads/fix-image"), want: foreignBranchDenial},
		{name: "empty branch suffix", body: makePush(zero, newOID, "refs/heads/sproozi/run-abc-uid/"), want: "invalid ref name"},
		{name: "ambiguous ref", body: makePush(zero, newOID, "refs/heads/sproozi/run-abc-uid/a..b"), want: "invalid ref name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := inspectReceivePack(io.NopCloser(bytes.NewReader(tt.body)), testRunUID)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("inspectReceivePack() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestInspectReceivePackRejectsMultiRefAtomically(t *testing.T) {
	zero := strings.Repeat("0", 40)
	newOID := strings.Repeat("a", 40)
	first := fmt.Sprintf("%s %s refs/heads/sproozi/run-abc-uid/one\x00report-status", zero, newOID)
	second := fmt.Sprintf("%s %s refs/heads/sproozi/run-abc-uid/two", zero, newOID)
	body := append(makePktLine(first), makePktLine(second)...)
	body = append(body, makeFlush()...)

	_, err := inspectReceivePack(io.NopCloser(bytes.NewReader(body)), testRunUID)
	if err == nil || !strings.Contains(err.Error(), "exactly one ref") {
		t.Fatalf("inspectReceivePack() error = %v, want multi-ref denial", err)
	}
}

func TestParsePushCommandsRejectsMalformedOrTruncatedFraming(t *testing.T) {
	zero := strings.Repeat("0", 40)
	newOID := strings.Repeat("a", 40)
	validCommand := fmt.Sprintf("%s %s refs/heads/sproozi/run-abc-uid/fix", zero, newOID)
	completePacket := makePktLine(validCommand)

	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "truncated header", body: []byte("003"), want: "truncated pkt-line header"},
		{name: "invalid hex", body: []byte("zzzz"), want: "invalid pkt-line length"},
		{name: "special packet", body: []byte("0001"), want: "invalid command pkt-line length"},
		{name: "flush without command", body: makeFlush(), want: "contains no ref command"},
		{name: "truncated payload", body: completePacket[:len(completePacket)-1], want: "truncated pkt-line payload"},
		{name: "missing flush", body: completePacket, want: "truncated pkt-line header"},
		{name: "malformed command", body: append(makePktLine("not-a-command"), makeFlush()...), want: "malformed receive-pack ref command"},
		{name: "invalid object ID", body: append(makePktLine("xyz "+newOID+" refs/heads/sproozi/run-abc-uid/fix"), makeFlush()...), want: "invalid object ID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePushCommands(&fragmentReader{data: tt.body}, maxReceivePackCommandBytes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parsePushCommands() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParsePushCommandsEnforcesCommandSectionBound(t *testing.T) {
	body := append(makePktLine(strings.Repeat("x", 128)), makeFlush()...)
	_, err := parsePushCommands(bytes.NewReader(body), 64)
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 bytes") {
		t.Fatalf("parsePushCommands() error = %v, want bounded-section denial", err)
	}
}
