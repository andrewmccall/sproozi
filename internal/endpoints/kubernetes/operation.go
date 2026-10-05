// Package kubernetesgateway implements the semantic kubernetes.read gateway.
package kubernetes

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	appsAPIGroup        = "apps"
	verbGet             = "get"
	coreDiscoveryPath   = "/api"
	coreV1DiscoveryPath = "/api/v1"
	groupsDiscoveryPath = "/apis"
	appsV1DiscoveryPath = "/apis/apps/v1"
)

// Operation is the normalized, bounded Kubernetes request understood by the gateway.
type Operation struct {
	Verb        string
	APIGroup    string
	Version     string
	Discovery   bool
	Namespace   string
	Resource    string
	Name        string
	Subresource string
	Watch       bool
}

// ParseOperation accepts only Kubernetes resource URLs, never arbitrary API paths.
func ParseOperation(r *http.Request) (Operation, error) {
	if r == nil || r.URL == nil {
		return Operation{}, fmt.Errorf("kubernetes: missing request")
	}
	if r.Method != http.MethodGet {
		return Operation{}, fmt.Errorf("kubernetes: only GET is permitted")
	}
	if r.URL.Opaque != "" || r.URL.RawPath != "" || strings.Contains(r.URL.Path, "..") {
		return Operation{}, fmt.Errorf("kubernetes: invalid path")
	}
	if r.URL.Path == "" || (r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/")) || strings.Contains(r.URL.Path, "//") {
		return Operation{}, fmt.Errorf("kubernetes: non-canonical path")
	}
	if operation, ok := discoveryOperation(r.URL); ok {
		return operation, nil
	}
	op, err := parseResourcePath(r.URL.Path)
	if err != nil {
		return Operation{}, err
	}
	if err := validateQuery(r.URL.Query(), &op); err != nil {
		return Operation{}, err
	}
	if op.Watch {
		op.Verb = "watch"
	} else if op.Name == "" {
		op.Verb = "list"
	}
	return op, nil
}

func parseResourcePath(path string) (Operation, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 {
		return Operation{}, fmt.Errorf("kubernetes: unsupported path")
	}
	op := Operation{Verb: verbGet, APIGroup: ""}
	i := 0
	switch parts[0] {
	case "api":
		if len(parts) < 2 {
			return Operation{}, fmt.Errorf("kubernetes: missing version")
		}
		op.Version, i = parts[1], 2
	case "apis":
		if len(parts) < 3 {
			return Operation{}, fmt.Errorf("kubernetes: missing group/version")
		}
		op.APIGroup, op.Version, i = parts[1], parts[2], 3
	default:
		return Operation{}, fmt.Errorf("kubernetes: unsupported API prefix")
	}
	if op.Version == "" || strings.ContainsAny(op.Version, "?/#") || i >= len(parts) {
		return Operation{}, fmt.Errorf("kubernetes: invalid version")
	}
	if parts[i] == "namespaces" {
		if len(parts) <= i+2 || parts[i+1] == "" {
			return Operation{}, fmt.Errorf("kubernetes: invalid namespace path")
		}
		op.Namespace, i = parts[i+1], i+2
	}
	if i >= len(parts) || parts[i] == "" {
		return Operation{}, fmt.Errorf("kubernetes: missing resource")
	}
	op.Resource, i = parts[i], i+1
	if i < len(parts) {
		op.Name, i = parts[i], i+1
	}
	if i < len(parts) {
		op.Subresource, i = parts[i], i+1
	}
	if i != len(parts) || strings.ContainsAny(op.Resource+op.Name+op.Subresource, "?/#") {
		return Operation{}, fmt.Errorf("kubernetes: unsupported resource path")
	}
	for _, part := range parts {
		if part == "" || part == "." {
			return Operation{}, fmt.Errorf("kubernetes: invalid path segment")
		}
	}
	if op.Name == "" && op.Subresource != "" {
		return Operation{}, fmt.Errorf("kubernetes: subresource requires name")
	}
	return op, nil
}

func discoveryOperation(u *url.URL) (Operation, bool) {
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) > 1 {
		return Operation{}, false
	}
	if len(q) == 1 {
		values, ok := q["timeout"]
		if !ok || len(values) != 1 {
			return Operation{}, false
		}
		duration, err := time.ParseDuration(values[0])
		if err != nil || duration <= 0 || duration > 300*time.Second {
			return Operation{}, false
		}
	}
	switch u.Path {
	case coreDiscoveryPath:
		return Operation{Verb: verbGet, Discovery: true}, true
	case coreV1DiscoveryPath:
		return Operation{Verb: verbGet, Version: "v1", Discovery: true}, true
	case groupsDiscoveryPath:
		return Operation{Verb: verbGet, Discovery: true}, true
	case "/apis/apps":
		return Operation{Verb: verbGet, APIGroup: appsAPIGroup, Discovery: true}, true
	case appsV1DiscoveryPath:
		return Operation{Verb: verbGet, APIGroup: appsAPIGroup, Version: "v1", Discovery: true}, true
	default:
		return Operation{}, false
	}
}

func validateQuery(q url.Values, op *Operation) error {
	allowed := map[string]bool{"watch": true, "resourceVersion": true, "resourceVersionMatch": true, "timeoutSeconds": true, "labelSelector": true, "fieldSelector": true, "limit": true, "continue": true, "tailLines": true, "previous": true, "timestamps": true, "container": true, "sinceSeconds": true, "sinceTime": true}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 {
			return fmt.Errorf("kubernetes: unsupported query parameter %q", key)
		}
	}
	if v := q.Get("watch"); v != "" {
		if v != "true" && v != "1" && v != "false" && v != "0" {
			return fmt.Errorf("kubernetes: invalid watch")
		}
		op.Watch = v == "true" || v == "1"
	}
	if v := q.Get("timeoutSeconds"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 300 {
			return fmt.Errorf("kubernetes: watch timeout is out of bounds")
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5000 {
			return fmt.Errorf("kubernetes: list limit is out of bounds")
		}
	}
	if v := q.Get("continue"); len(v) > 2048 {
		return fmt.Errorf("kubernetes: continue token is too long")
	}
	if len(q.Get("labelSelector")) > 2048 || len(q.Get("fieldSelector")) > 2048 {
		return fmt.Errorf("kubernetes: selector is too long")
	}
	if op.Watch && op.Name != "" {
		return fmt.Errorf("kubernetes: named watches are not permitted")
	}
	if q.Get("resourceVersionMatch") != "" && q.Get("resourceVersion") == "" {
		return fmt.Errorf("kubernetes: resourceVersionMatch requires resourceVersion")
	}
	return nil
}
