// Package mcppolicy validates declarative argument restrictions at admission
// and at the remote MCP request boundary. It does not infer service semantics.
package mcppolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type Constraint struct {
	raw      json.RawMessage
	resolved *jsonschema.Resolved
}

// Compile rejects unsupported restrictions instead of silently ignoring them.
// Remote references, format assertions and defaults are not policy mechanisms.
func Compile(raw []byte) (*Constraint, error) {
	if len(raw) == 0 || len(raw) > 32<<10 {
		return nil, fmt.Errorf("MCP argument schema exceeds bounds")
	}
	if _, err := DecodeObject(raw); err != nil {
		return nil, fmt.Errorf("MCP argument schema must be an unambiguous object")
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("invalid MCP argument schema")
	}
	if err := CheckSchema(&schema); err != nil {
		return nil, err
	}
	if err := checkKeywords(&schema, 0); err != nil {
		return nil, err
	}
	resolved, err := schema.Resolve(nil) // No remote loader and no defaults.
	if err != nil {
		return nil, fmt.Errorf("unsupported MCP argument schema")
	}
	return &Constraint{raw: bytes.Clone(raw), resolved: resolved}, nil
}

func (c *Constraint) Validate(value any) error { return c.resolved.Validate(value) }
func (c *Constraint) Schema() json.RawMessage  { return bytes.Clone(c.raw) }

// CompileScope is shared by admission and the live gateway boundary.
func CompileScope(scope api.MCPServerScope) (map[string]*Constraint, error) {
	if len(scope.Tools) == 0 || len(scope.Tools) > 128 {
		return nil, fmt.Errorf("MCP scope requires bounded explicit tools")
	}
	result := make(map[string]*Constraint, len(scope.Tools))
	for name, rule := range scope.Tools {
		if !toolName.MatchString(name) {
			return nil, fmt.Errorf("invalid MCP tool name")
		}
		var constraint *Constraint
		if rule.Arguments != nil {
			var err error
			constraint, err = Compile(rule.Arguments.Raw)
			if err != nil {
				return nil, err
			}
		}
		result[name] = constraint
	}
	return result, nil
}

// DecodeObject parses once so the validated and forwarded values agree. Reject
// duplicate keys and excessive nesting, including within arrays and objects.
func DecodeObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, fmt.Errorf("MCP JSON exceeds bounds")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := inspect(d, 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("MCP JSON has trailing data")
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, fmt.Errorf("MCP arguments must be an object")
	}
	return value, nil
}

func inspect(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("MCP JSON nesting exceeds bounds")
	}
	token, err := d.Token()
	if err != nil {
		return fmt.Errorf("invalid MCP JSON")
	}
	if number, ok := token.(json.Number); ok {
		// The schema library uses float64. Reject values whose decimal meaning
		// would change when decoded and forwarded, including rounded IDs.
		value, err := number.Float64()
		if err != nil || len(number.String()) > 128 {
			return fmt.Errorf("MCP number exceeds supported precision")
		}
		normalized, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("MCP number exceeds supported precision")
		}
		// Avoid constructing a huge rational for an underflowed exponent.
		if value == 0 {
			for _, digit := range strings.Split(strings.ToLower(number.String()), "e")[0] {
				if digit >= '1' && digit <= '9' {
					return fmt.Errorf("MCP number exceeds supported precision")
				}
			}
		} else {
			original, ok := new(big.Rat).SetString(number.String())
			roundTrip, valid := new(big.Rat).SetString(string(normalized))
			if !ok || !valid || original.Cmp(roundTrip) != 0 {
				return fmt.Errorf("MCP number exceeds supported precision")
			}
		}
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return fmt.Errorf("invalid MCP JSON")
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid MCP JSON key")
			}
			seen[name] = true
		}
		if err := inspect(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// Traverse schema-valued fields using the library's own data structure, rather
// than maintain a second list of JSON Schema keywords or accidentally inspect
// user values within const/enum as schemas.
func checkKeywords(s *jsonschema.Schema, depth int) error {
	if s == nil {
		return nil
	}
	if depth > 32 || len(s.Extra) != 0 || s.Format != "" || s.ContentSchema != nil || len(s.Vocabulary) != 0 {
		return fmt.Errorf("unsupported MCP schema restriction")
	}
	for _, child := range schemaChildren(s) {
		if err := checkKeywords(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}
