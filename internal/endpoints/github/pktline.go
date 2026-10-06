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
	"strconv"
	"strings"
)

const maxReceivePackCommandBytes = 64 << 10

type pushCommand struct {
	oldOID  string
	newOID  string
	refName string
}

// inspectReceivePack parses the complete command section without buffering the
// packfile. Bytes consumed during inspection are restored for upstream
// forwarding. A flush packet is mandatory: EOF is never an implicit terminator.
func inspectReceivePack(body io.ReadCloser, runUID string) (io.ReadCloser, error) {
	var inspected bytes.Buffer
	commands, err := parsePushCommands(io.TeeReader(body, &inspected), maxReceivePackCommandBytes)
	restored := io.NopCloser(io.MultiReader(bytes.NewReader(inspected.Bytes()), body))
	if err != nil {
		return restored, err
	}
	if err := authorizePushCommands(commands, runUID); err != nil {
		return restored, err
	}
	return restored, nil
}

func parsePushCommands(reader io.Reader, maxBytes int) ([]pushCommand, error) {
	var commands []pushCommand
	consumed := 0
	for {
		if consumed+4 > maxBytes {
			return nil, fmt.Errorf("receive-pack command section exceeds %d bytes", maxBytes)
		}

		header := make([]byte, 4)
		if _, err := io.ReadFull(reader, header); err != nil {
			return nil, fmt.Errorf("truncated pkt-line header: %w", err)
		}
		consumed += 4

		packetLength, err := strconv.ParseUint(string(header), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid pkt-line length %q", header)
		}
		if packetLength == 0 {
			if len(commands) == 0 {
				return nil, fmt.Errorf("receive-pack contains no ref command")
			}
			return commands, nil
		}
		if packetLength <= 4 {
			return nil, fmt.Errorf("invalid command pkt-line length %d", packetLength)
		}
		payloadLength := int(packetLength) - 4
		if consumed+payloadLength > maxBytes {
			return nil, fmt.Errorf("receive-pack command section exceeds %d bytes", maxBytes)
		}
		payload := make([]byte, payloadLength)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, fmt.Errorf("truncated pkt-line payload: %w", err)
		}
		consumed += payloadLength

		command, err := parsePushCommand(payload, len(commands) == 0)
		if err != nil {
			return nil, err
		}
		commands = append(commands, command)
	}
}

func parsePushCommand(payload []byte, first bool) (pushCommand, error) {
	payload = bytes.TrimSuffix(payload, []byte{'\n'})
	if bytes.ContainsAny(payload, "\r\n") {
		return pushCommand{}, fmt.Errorf("receive-pack command contains an embedded line break")
	}

	commandBytes := payload
	if before, after, ok := bytes.Cut(payload, []byte{0}); ok {
		if !first {
			return pushCommand{}, fmt.Errorf("capabilities are only valid on the first ref command")
		}
		commandBytes = before
		capabilities := after
		if bytes.IndexByte(capabilities, 0) >= 0 {
			return pushCommand{}, fmt.Errorf("receive-pack capabilities contain NUL")
		}
		for _, b := range capabilities {
			if b < 0x20 || b > 0x7e {
				return pushCommand{}, fmt.Errorf("receive-pack capabilities contain invalid bytes")
			}
		}
	}

	parts := strings.Split(string(commandBytes), " ")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return pushCommand{}, fmt.Errorf("malformed receive-pack ref command")
	}
	if !validObjectID(parts[0]) || !validObjectID(parts[1]) || len(parts[0]) != len(parts[1]) {
		return pushCommand{}, fmt.Errorf("receive-pack command has invalid object ID")
	}
	if !validRefName(parts[2]) {
		return pushCommand{}, fmt.Errorf("receive-pack command has invalid ref name")
	}
	return pushCommand{oldOID: parts[0], newOID: parts[1], refName: parts[2]}, nil
}

func authorizePushCommands(commands []pushCommand, runUID string) error {
	if len(commands) != 1 {
		return fmt.Errorf("receive-pack must create exactly one ref")
	}
	if runUID == "" || strings.Contains(runUID, "/") {
		return fmt.Errorf("run identity cannot derive a branch namespace")
	}

	command := commands[0]
	if allZero(command.newOID) {
		return fmt.Errorf("ref deletion is not permitted")
	}
	if !allZero(command.oldOID) {
		return fmt.Errorf("ref updates are not permitted; create a new run-owned branch")
	}
	requiredPrefix := "refs/heads/" + runHeadBranchPrefix(runUID)
	if !strings.HasPrefix(command.refName, requiredPrefix) || len(command.refName) == len(requiredPrefix) {
		return fmt.Errorf("ref %q is not owned by this run", command.refName)
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func allZero(value string) bool { return strings.Trim(value, "0") == "" }

// validRefName implements the security-relevant Git check-ref-format rules so
// an accepted run prefix cannot hide a different or ambiguous ref spelling.
func validRefName(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	for _, c := range ref {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return false
		}
	}
	for component := range strings.SplitSeq(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}
