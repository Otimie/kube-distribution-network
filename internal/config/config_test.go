package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shipped example must stay loadable, or the first thing a new user runs
// fails.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if len(cfg.Rules) == 0 {
		t.Fatal("example config has no rules")
	}
	for _, r := range cfg.Rules {
		if r.Resource == "secrets" || r.Resource == "serviceaccounts" {
			t.Errorf("the shipped example allowlists %q; it must not publish credentials", r.Resource)
		}
	}
}

// The config embedded in the deployment manifest is a copy of the example, and
// copies drift. Load it through the real parser so drift fails the build.
func TestEmbeddedManifestConfigLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kubernetes.yaml"))
	if err != nil {
		t.Fatalf("read manifests: %v", err)
	}

	var embedded string
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
		}
		if dec.Decode(&doc) != nil {
			break
		}
		if doc.Kind == "ConfigMap" && doc.Data["config.yaml"] != "" {
			embedded = doc.Data["config.yaml"]
		}
	}
	if embedded == "" {
		t.Fatal("no config.yaml found in any ConfigMap in deploy/kubernetes.yaml")
	}

	tmp := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmp, []byte(embedded), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(tmp); err != nil {
		t.Fatalf("the config embedded in deploy/kubernetes.yaml does not load: %v", err)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	// A typo must fail loudly rather than silently leave a setting at default.
	if _, err := loadString(t, `
rules:
  - version: v1
    resource: nodes
    ttl: "30s"
    namespacs: ["x"]
`); err == nil {
		t.Error("a misspelled config key should be rejected")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"empty rules": `
upstreamConcurrency: 4
`,
		"missing ttl": `
rules:
  - version: v1
    resource: nodes
`,
		"missing version": `
rules:
  - resource: nodes
    ttl: "30s"
`,
		"duplicate rule": `
rules:
  - version: v1
    resource: nodes
    ttl: "30s"
  - version: v1
    resource: nodes
    ttl: "60s"
`,
		"tls cert without key": `
tls:
  certFile: /tls/tls.crt
rules:
  - version: v1
    resource: nodes
    ttl: "30s"
`,
		"mtls without tls": `
tls:
  clientCAFile: /tls/ca.crt
rules:
  - version: v1
    resource: nodes
    ttl: "30s"
`,
	}
	for name, body := range cases {
		if _, err := loadString(t, body); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestCoreGroupAlias(t *testing.T) {
	cfg, err := loadString(t, `
rules:
  - group: core
    version: v1
    resource: nodes
    ttl: "30s"
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[0].Group != "" {
		t.Errorf("group = %q, want \"\" (the \"core\" alias should normalize)", cfg.Rules[0].Group)
	}
}

func loadString(t *testing.T, body string) (*Config, error) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(tmp)
}
