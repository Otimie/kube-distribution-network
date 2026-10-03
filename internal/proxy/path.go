package proxy

import (
	"fmt"
	"strings"
)

// target is a parsed Kubernetes API request path.
type target struct {
	// path is the raw request path, needed to vet non-resource paths.
	path string

	// discovery marks a non-resource path: /api, /apis, /version, /openapi/...
	discovery bool

	group       string // "" is the core group
	version     string
	namespace   string // "" means cluster-wide, or a cluster-scoped resource
	resource    string
	name        string
	subresource string
}

// route is a low-cardinality label for logging; it omits namespace and name so
// a caller enumerating names cannot blow up log volume.
func (t *target) route() string {
	if t.discovery {
		return "discovery"
	}
	g := t.group
	if g == "" {
		g = "core"
	}
	return g + "/" + t.version + "/" + t.resource
}

// namespaceSubresources are the segments that, after /namespaces/<name>/, mean a
// subresource of the namespace rather than a namespaced collection. The API
// server hardcodes the same set.
var namespaceSubresources = map[string]bool{"finalize": true, "status": true}

// parsePath splits a Kubernetes API path. The shapes, for both /api/<version>
// and /apis/<group>/<version>:
//
//	.../<resource>[/<name>[/<subresource>]]                 cluster scope
//	.../namespaces/<ns>/<resource>[/<name>[/<subresource>]] namespaced
func parsePath(path string) (*target, error) {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		return &target{path: path, discovery: true}, nil
	}

	t := &target{path: path}
	switch parts[0] {
	case "api":
		if len(parts) < 2 {
			return &target{path: path, discovery: true}, nil // /api
		}
		t.version = parts[1]
		parts = parts[2:]
	case "apis":
		if len(parts) < 3 {
			return &target{path: path, discovery: true}, nil // /apis, /apis/<group>
		}
		t.group, t.version = parts[1], parts[2]
		parts = parts[3:]
	default:
		return &target{path: path, discovery: true}, nil // /version, /openapi/..., /healthz
	}

	if len(parts) == 0 {
		return &target{path: path, discovery: true}, nil // /api/v1, /apis/<group>/<version>
	}

	// "/api/v1/namespaces/foo" is a namespace object, while
	// "/api/v1/namespaces/foo/pods" is a collection inside that namespace.
	if parts[0] == "namespaces" && len(parts) >= 3 && !namespaceSubresources[parts[2]] {
		t.namespace = parts[1]
		parts = parts[2:]
	}

	t.resource = parts[0]
	if len(parts) >= 2 {
		t.name = parts[1]
	}
	if len(parts) >= 3 {
		t.subresource = parts[2]
	}
	if len(parts) > 3 {
		return nil, fmt.Errorf("unrecognized path %q", path)
	}
	return t, nil
}
