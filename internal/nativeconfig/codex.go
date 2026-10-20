// Package nativeconfig validates trusted stock-client settings without depending
// on runtime, API, checkpoint or transport packages.
package nativeconfig

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
)

// Validate accepts bounded administrator-selected Codex launch settings. Values
// remain native TOML, interpreted by the native client; this is not a TOML parser.
// Keys use a restricted dotted grammar so alternative spellings cannot replace
// the separately owned developer instructions or startup update gate.
func Validate(model string, options []string) error {
	if len(model) > 128 || strings.ContainsAny(model, "\x00\r\n") || len(options) > 32 {
		return errors.New("invalid managed native model or option count")
	}
	for _, option := range options {
		if len(option) > 4096 || strings.ContainsAny(option, "\x00\r\n") {
			return errors.New("native option must be bounded and single-line")
		}
		key, err := assignmentKey(option)
		if err != nil {
			return err
		}
		switch {
		case key[0] == "developer_instructions", key[0] == "check_for_update_on_startup",
			// The Sproozi control connection follows these options; a whole-table
			// or sibling assignment could still disable or redirect it.
			key[0] == "mcp_servers" && (len(key) == 1 || key[1] == ControlMCPServer):
			return errors.New("native option cannot replace a reserved setting")
		}
	}
	return nil
}

// ControlMCPServer names the Sproozi control connection. The API re-exports it;
// it is defined here because this package cannot import the API.
const ControlMCPServer = "sproozi"

func assignmentKey(option string) ([]string, error) {
	var key []string
	for offset := 0; ; {
		name, end, err := segment(option, offset)
		if err != nil {
			return nil, err
		}
		key = append(key, name)
		if end == len(option) {
			return nil, errors.New("native option requires an assignment")
		}
		switch option[end] {
		case '.':
			offset = end + 1
		case '=':
			if strings.TrimSpace(option[end+1:]) == "" {
				return nil, errors.New("native option requires a value")
			}
			return key, nil
		default:
			return nil, errors.New("native option key has unsupported syntax")
		}
	}
}

func segment(option string, start int) (string, int, error) {
	if start >= len(option) {
		return "", start, errors.New("native option key segment is empty")
	}
	if option[start] == '"' {
		return quotedSegment(option, start)
	}
	end := start
	for end < len(option) && bareCharacter(option[end]) {
		end++
	}
	if start == end {
		return "", end, errors.New("native option key requires a bare or double-quoted segment")
	}
	return option[start:end], end, nil
}

func bareCharacter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func quotedSegment(option string, start int) (string, int, error) {
	for end := start + 1; end < len(option); end++ {
		if option[end] == '\\' {
			end++
			continue
		}
		if option[end] != '"' {
			continue
		}
		key, err := strconv.Unquote(option[start : end+1])
		if err != nil || key == "" || strings.ContainsFunc(key, unicode.IsControl) {
			return "", end, errors.New("native quoted key is invalid")
		}
		return key, end + 1, nil
	}
	return "", len(option), errors.New("native quoted key is incomplete")
}
