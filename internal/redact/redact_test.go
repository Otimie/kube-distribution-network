package redact

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, paths ...string) []*Path {
	t.Helper()
	out := make([]*Path, 0, len(paths))
	for _, s := range paths {
		p, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		out = append(out, p)
	}
	return out
}

func apply(t *testing.T, body string, list bool, paths ...string) string {
	t.Helper()
	out, err := Apply([]byte(body), mustParse(t, paths...), list)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{
		"",
		"[*]",                    // the root is an object
		"spec..containers",       // empty key
		"spec.containers[",       // unclosed
		"spec.containers[name]",  // neither [*] nor [key: pattern]
		"spec.containers[name:]", // empty pattern
		"spec.containers]x",      // stray ]
		`metadata."unclosed`,     // unclosed quote
		`metadata.""`,            // empty quoted key
		"spec.containersfoo[*]x", // junk after a selector
		"kind",                   // clients need these
		"apiVersion",
		"metadata",
		"metadata.name",
		"metadata.namespace",
		`"metadata"."name"`,
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q): expected an error", s)
		}
	}
}

func TestRemovesFields(t *testing.T) {
	node := `{"kind":"Node","metadata":{"name":"n1","managedFields":[{"manager":"kubelet"}]},` +
		`"status":{"images":[{"names":["nginx"],"sizeBytes":123456789012}],"capacity":{"cpu":"4"}}}`
	got := apply(t, node, false, "status.images", "metadata.managedFields", "status.notThere", "spec.missing.deeper")
	want := `{"kind":"Node","metadata":{"name":"n1"},"status":{"capacity":{"cpu":"4"}}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRemovesFromListItems(t *testing.T) {
	list := `{"kind":"NodeList","metadata":{"resourceVersion":"9"},"items":[` +
		`{"metadata":{"name":"a"},"status":{"images":[]}},{"metadata":{"name":"b"},"status":{"images":[]}}]}`
	got := apply(t, list, true, "status.images")
	want := `{"items":[{"metadata":{"name":"a"},"status":{}},{"metadata":{"name":"b"},"status":{}}],` +
		`"kind":"NodeList","metadata":{"resourceVersion":"9"}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	if got := apply(t, `{"kind":"NodeList","items":null}`, true, "status.images"); got != `{"items":null,"kind":"NodeList"}` {
		t.Errorf("null items: got %s", got)
	}
}

func TestSelectors(t *testing.T) {
	pod := `{"spec":{"containers":[` +
		`{"name":"app","args":["--token=x"],"env":[{"name":"DB_PASSWORD","value":"p"},{"name":"MODE","value":"prod"}]},` +
		`{"name":"sidecar","env":[{"name":"DB_USER","value":"u"},{"value":"no-name"}]}]}}`

	for _, tc := range []struct {
		path, want string
	}{
		{"spec.containers[*].args",
			`{"spec":{"containers":[{"env":[{"name":"DB_PASSWORD","value":"p"},{"name":"MODE","value":"prod"}],"name":"app"},` +
				`{"env":[{"name":"DB_USER","value":"u"},{"value":"no-name"}],"name":"sidecar"}]}}`},
		// Items without the key never match.
		{"spec.containers[name: *].env[name: *].value",
			`{"spec":{"containers":[{"args":["--token=x"],"env":[{"name":"DB_PASSWORD"},{"name":"MODE"}],"name":"app"},` +
				`{"env":[{"name":"DB_USER"},{"value":"no-name"}],"name":"sidecar"}]}}`},
		// Ending at a selector drops the matched items.
		{"spec.containers[name: app].env[name: DB_*]",
			`{"spec":{"containers":[{"args":["--token=x"],"env":[{"name":"MODE","value":"prod"}],"name":"app"},` +
				`{"env":[{"name":"DB_USER","value":"u"},{"value":"no-name"}],"name":"sidecar"}]}}`},
		{"spec.containers[name: side*]",
			`{"spec":{"containers":[{"args":["--token=x"],"env":[{"name":"DB_PASSWORD","value":"p"},{"name":"MODE","value":"prod"}],"name":"app"}]}}`},
	} {
		if got := apply(t, pod, false, tc.path); got != tc.want {
			t.Errorf("%s:\ngot  %s\nwant %s", tc.path, got, tc.want)
		}
	}
}

func TestQuotedKeys(t *testing.T) {
	obj := `{"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{...}","keep":"yes"}}}`
	got := apply(t, obj, false, `metadata.annotations."kubectl.kubernetes.io/last-applied-configuration"`)
	if want := `{"metadata":{"annotations":{"keep":"yes"}}}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// A path that meets the wrong kind of value no longer describes the data, so
// the response must not be served.
func TestWrongShapeFailsClosed(t *testing.T) {
	for _, tc := range []struct{ body, path string }{
		{`{"spec":{"containers":{"name":"app"}}}`, "spec.containers[*].env"},
		{`{"spec":{"containers":["app"]}}`, "spec.containers[name: *].env"},
		{`{"spec":["x"]}`, "spec.containers"},
		{`{"status":"ok"}`, "status.images"},
	} {
		if _, err := Apply([]byte(tc.body), mustParse(t, tc.path), false); err == nil {
			t.Errorf("%s on %s: expected an error", tc.path, tc.body)
		}
	}
	for _, body := range []string{`not json`, `[]`, `{"items":{}}`} {
		if _, err := Apply([]byte(body), mustParse(t, "status.images"), true); err == nil {
			t.Errorf("list %s: expected an error", body)
		}
	}
}

func TestKeepsNumbersAndHTML(t *testing.T) {
	got := apply(t, `{"n":12345678901234567890,"f":1.50,"s":"<a&b>","x":1}`, false, "x")
	if want := `{"f":1.50,"n":12345678901234567890,"s":"<a&b>"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true}, {"*", "x", true}, {"a", "a", true}, {"a", "ab", false},
		{"DB_*", "DB_USER", true}, {"DB_*", "XDB_USER", false},
		{"*_KEY", "API_KEY", true}, {"a*b*c", "aXbYc", true}, {"a*b*c", "aXcYb", false},
		{"ab*ba", "aba", false},
	} {
		if got := glob(tc.pattern, tc.s); got != tc.want {
			t.Errorf("glob(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}
