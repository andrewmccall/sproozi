package harness

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tomlDottedKey reads the TOML dotted key at the start of s: bare, basic
// ("...", with escapes) or literal ('...') parts joined by ".", with spaces
// or tabs around each. It returns each part with its escapes decoded and its
// surrounding whitespace trimmed, so that a spelling that differs only there
// compares equal to the plain key, and the rest of s after the key. ok is
// false when s does not start with a well-formed dotted key.
func tomlDottedKey(s string) (parts []string, rest string, ok bool) {
	i := 0
	blank := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	for {
		blank()
		if i >= len(s) {
			return nil, "", false
		}
		var part string
		switch s[i] {
		case '"':
			end := i + 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(s) {
				return nil, "", false
			}
			decoded, decodedOK := tomlBasicString(s[i+1 : end])
			if !decodedOK {
				return nil, "", false
			}
			part, i = decoded, end+1
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, "", false
			}
			part, i = s[i+1:i+1+end], i+end+2
		default:
			start := i
			for i < len(s) && tomlBareKeyByte(s[i]) {
				i++
			}
			if i == start {
				return nil, "", false
			}
			part = s[start:i]
		}
		parts = append(parts, strings.TrimSpace(part))
		blank()
		if i < len(s) && s[i] == '.' {
			i++
			continue
		}
		return parts, s[i:], true
	}
}

// tomlQuote spells s as a TOML basic string, which tomlBasicString reads back
// as s.
func tomlQuote(s string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&out, "\\u%04X", r)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}

func tomlBareKeyByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// tomlBasicString decodes the body of a TOML basic string, refusing an
// unknown escape or one that names no Unicode scalar value.
func tomlBasicString(body string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			out.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case 'b':
			out.WriteByte('\b')
		case 't':
			out.WriteByte('\t')
		case 'n':
			out.WriteByte('\n')
		case 'f':
			out.WriteByte('\f')
		case 'r':
			out.WriteByte('\r')
		case 'e':
			out.WriteByte('\x1b')
		case '"', '\\':
			out.WriteByte(body[i])
		case 'u', 'U':
			width := 4
			if body[i] == 'U' {
				width = 8
			}
			if i+1+width > len(body) {
				return "", false
			}
			code, err := strconv.ParseUint(body[i+1:i+1+width], 16, 32)
			if err != nil || !utf8.ValidRune(rune(code)) {
				return "", false
			}
			out.WriteRune(rune(code))
			i += width
		default:
			return "", false
		}
	}
	return out.String(), utf8.ValidString(out.String())
}
