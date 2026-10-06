package mcppolicy

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// CheckSchema bounds evaluation before passing external schemas to the validator.
// Only acyclic local JSON Pointer references are supported. Dynamic references,
// anchors and nested resource IDs would require a different evaluation model.
func CheckSchema(root *jsonschema.Schema) error {
	nodes := map[string]*jsonschema.Schema{}
	edges := map[*jsonschema.Schema][]*jsonschema.Schema{}
	var visit func(*jsonschema.Schema, string, int) error
	visit = func(s *jsonschema.Schema, path string, depth int) error {
		if s == nil {
			return nil
		}
		if depth > 32 || len(nodes) >= 256 || s.DynamicRef != "" || s.Anchor != "" || s.DynamicAnchor != "" || (path != "#" && s.ID != "") {
			return fmt.Errorf("MCP schema exceeds supported reference bounds")
		}
		switch s.Schema {
		case "", "https://json-schema.org/draft/2020-12/schema", "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#":
		default:
			return fmt.Errorf("unsupported MCP schema dialect")
		}
		nodes[path] = s
		for name, child := range schemaChildren(s) {
			if child != nil {
				edges[s] = append(edges[s], child)
				if err := visit(child, path+"/"+name, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(root, "#", 0); err != nil {
		return err
	}
	for _, s := range nodes {
		if s.Ref == "" {
			continue
		}
		reference, err := url.Parse(s.Ref)
		if err != nil || !strings.HasPrefix(s.Ref, "#") {
			return fmt.Errorf("MCP schema requires local references")
		}
		target := nodes["#"+reference.Fragment]
		if target == nil {
			return fmt.Errorf("unsupported MCP schema reference")
		}
		edges[s] = append(edges[s], target)
	}
	active := map[*jsonschema.Schema]bool{}
	costs := map[*jsonschema.Schema]int{}
	var count func(*jsonschema.Schema) (int, error)
	count = func(s *jsonschema.Schema) (int, error) {
		if active[s] {
			return 0, fmt.Errorf("cyclic MCP schema reference")
		}
		if cost, ok := costs[s]; ok {
			return cost, nil
		}
		active[s] = true
		cost := 1
		for _, child := range edges[s] {
			n, err := count(child)
			if err != nil {
				return 0, err
			}
			cost += n
			if cost > 4096 {
				return 0, fmt.Errorf("MCP schema evaluation exceeds bounds")
			}
		}
		active[s] = false
		costs[s] = cost
		return cost, nil
	}
	_, err := count(root)
	return err
}

// Use the library's schema-valued fields to find children. Values in const and
// enum are data, not schemas; JSON Pointer escaping preserves property names.
func schemaChildren(s *jsonschema.Schema) map[string]*jsonschema.Schema {
	result := map[string]*jsonschema.Schema{}
	v := reflect.ValueOf(s).Elem()
	target := reflect.TypeFor[*jsonschema.Schema]()
	for i := range v.NumField() {
		field := v.Field(i)
		name := strings.Split(v.Type().Field(i).Tag.Get("json"), ",")[0]
		escape := func(key string) string { return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1") }
		switch {
		case field.Type() == target:
			result[name] = field.Interface().(*jsonschema.Schema)
		case field.Kind() == reflect.Slice && field.Type().Elem() == target:
			for j := range field.Len() {
				result[name+"/"+strconv.Itoa(j)] = field.Index(j).Interface().(*jsonschema.Schema)
			}
		case field.Kind() == reflect.Map && field.Type().Elem() == target:
			iter := field.MapRange()
			for iter.Next() {
				result[name+"/"+escape(iter.Key().String())] = iter.Value().Interface().(*jsonschema.Schema)
			}
		}
	}
	return result
}
