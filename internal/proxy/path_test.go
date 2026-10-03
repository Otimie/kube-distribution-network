package proxy

import "testing"

func TestParsePath(t *testing.T) {
	tests := []struct {
		path string
		want target
	}{
		{"/api/v1/pods", target{version: "v1", resource: "pods"}},
		{"/api/v1/nodes/node-1", target{version: "v1", resource: "nodes", name: "node-1"}},
		{"/api/v1/namespaces/default/pods", target{version: "v1", namespace: "default", resource: "pods"}},
		{"/api/v1/namespaces/default/pods/web-0", target{version: "v1", namespace: "default", resource: "pods", name: "web-0"}},
		{"/api/v1/namespaces/default/pods/web-0/log", target{version: "v1", namespace: "default", resource: "pods", name: "web-0", subresource: "log"}},

		// The ambiguous namespace shapes.
		{"/api/v1/namespaces", target{version: "v1", resource: "namespaces"}},
		{"/api/v1/namespaces/kube-system", target{version: "v1", resource: "namespaces", name: "kube-system"}},
		{"/api/v1/namespaces/kube-system/status", target{version: "v1", resource: "namespaces", name: "kube-system", subresource: "status"}},
		{"/api/v1/namespaces/kube-system/configmaps", target{version: "v1", namespace: "kube-system", resource: "configmaps"}},

		{"/apis/apps/v1/deployments", target{group: "apps", version: "v1", resource: "deployments"}},
		{"/apis/apps/v1/namespaces/prod/deployments/api", target{group: "apps", version: "v1", namespace: "prod", resource: "deployments", name: "api"}},

		// Discovery and everything off the API surface.
		{"/api", target{discovery: true}},
		{"/api/v1", target{discovery: true}},
		{"/apis", target{discovery: true}},
		{"/apis/apps", target{discovery: true}},
		{"/apis/apps/v1", target{discovery: true}},
		{"/version", target{discovery: true}},
		{"/healthz", target{discovery: true}},
		{"/", target{discovery: true}},
	}

	for _, tc := range tests {
		tc.want.path = tc.path
		got, err := parsePath(tc.path)
		if err != nil {
			t.Errorf("parsePath(%q): %v", tc.path, err)
			continue
		}
		if *got != tc.want {
			t.Errorf("parsePath(%q)\n got  %+v\n want %+v", tc.path, *got, tc.want)
		}
	}
}

func TestParsePathRejectsOverlongPaths(t *testing.T) {
	if _, err := parsePath("/api/v1/pods/web-0/log/extra"); err == nil {
		t.Error("an over-long path should be rejected")
	}
}
