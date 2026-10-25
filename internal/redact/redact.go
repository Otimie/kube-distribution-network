// Package redact removes fields from Kubernetes objects before they are
// served, for rules that publish a resource but not all of its contents.
//
// A path names the fields to remove, from the object's root:
//
//	status.images                                  a field
//	metadata.annotations."kubernetes.io/change"    a key containing dots, quoted
//	spec.containers[*].args                        a field of every list item
//	spec.containers[name: *].env[name: DB_*]       list items matched by a key
//
// A selector matches list items, so ending a path with one removes the items
// themselves. [key: pattern] matches items whose key is a string matching the
// pattern, where "*" matches any run of characters; items without the key
// never match. [*] matches every item.
//
// Removal fails closed. A path that runs into a missing or null field removes
// nothing, since absent data has nothing to leak. A path that finds the wrong
// kind of value, such as a list where it expects an object, returns an error:
// the rule no longer describes the data, and serving it could leak.
package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Path is a parsed field path.
type Path struct {
	raw   string
	steps []step
}

// step is one hop: an object key, or a selector over list items.
type step struct {
	key string // object key; empty for a selector

	selector bool
	matchKey string // "" for [*]
	pattern  string
}

func (p *Path) String() string { return p.raw }

// protected are fields clients cannot do without: removing them would break
// decoding or identify nothing, rather than hide data.
var protected = map[string]bool{
	"kind": true, "apiVersion": true, "metadata": true,
	"metadata.name": true, "metadata.namespace": true,
}

// Parse parses a path.
func Parse(s string) (*Path, error) {
	p := &Path{raw: s}
	rest := strings.TrimSpace(s)
	if rest == "" {
		return nil, errors.New("empty path")
	}

	for first := true; rest != ""; first = false {
		switch {
		case rest[0] == '[':
			if first {
				return nil, fmt.Errorf("path %q: starts with a selector; an object is not a list", s)
			}
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q: unclosed [", s)
			}
			st, err := parseSelector(rest[1:end])
			if err != nil {
				return nil, fmt.Errorf("path %q: %w", s, err)
			}
			p.steps = append(p.steps, st)
			rest = rest[end+1:]

		default:
			if !first {
				if rest[0] != '.' {
					return nil, fmt.Errorf("path %q: expected . or [ before %q", s, rest)
				}
				rest = rest[1:]
			}
			key, n, err := parseKey(rest)
			if err != nil {
				return nil, fmt.Errorf("path %q: %w", s, err)
			}
			p.steps = append(p.steps, step{key: key})
			rest = rest[n:]
		}
	}

	keys := make([]string, 0, len(p.steps))
	for _, st := range p.steps {
		if st.selector {
			return p, nil
		}
		keys = append(keys, st.key)
	}
	if protected[strings.Join(keys, ".")] {
		return nil, fmt.Errorf("path %q: clients need this field; it cannot be removed", s)
	}
	return p, nil
}

// parseKey reads a bare or quoted key from the start of s and returns it with
// the number of bytes consumed.
func parseKey(s string) (string, int, error) {
	if strings.HasPrefix(s, `"`) {
		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return "", 0, errors.New(`unclosed "`)
		}
		if end == 0 {
			return "", 0, errors.New("empty key")
		}
		return s[1 : end+1], end + 2, nil
	}
	n := strings.IndexAny(s, `.["] `)
	if n < 0 {
		n = len(s)
	}
	if n == 0 {
		return "", 0, fmt.Errorf("expected a key at %q", s)
	}
	return s[:n], n, nil
}

func parseSelector(s string) (step, error) {
	s = strings.TrimSpace(s)
	if s == "*" {
		return step{selector: true}, nil
	}
	key, pattern, ok := strings.Cut(s, ":")
	key, pattern = strings.TrimSpace(key), strings.TrimSpace(pattern)
	if !ok || key == "" || pattern == "" {
		return step{}, fmt.Errorf("selector [%s] is not [*] or [key: pattern]", s)
	}
	return step{selector: true, matchKey: key, pattern: pattern}, nil
}

// Remove deletes the path's fields from a decoded JSON value, in place where
// it can, and returns the result.
func (p *Path) Remove(v any) (any, error) {
	return p.remove(v, p.steps, "")
}

func (p *Path) remove(v any, steps []step, at string) (any, error) {
	if v == nil {
		return nil, nil
	}
	st, last := steps[0], len(steps) == 1

	if !st.selector {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, p.mismatch(at, "an object", v)
		}
		child, ok := obj[st.key]
		if !ok {
			return v, nil
		}
		if last {
			delete(obj, st.key)
			return v, nil
		}
		next, err := p.remove(child, steps[1:], join(at, st.key))
		if err != nil {
			return nil, err
		}
		obj[st.key] = next
		return v, nil
	}

	list, ok := v.([]any)
	if !ok {
		return nil, p.mismatch(at, "a list", v)
	}
	kept := list[:0]
	for i, item := range list {
		matched, err := p.matches(st, item, fmt.Sprintf("%s[%d]", at, i))
		if err != nil {
			return nil, err
		}
		switch {
		case !matched:
			kept = append(kept, item)
		case last:
			// The path ends at the items: drop them.
		default:
			next, err := p.remove(item, steps[1:], fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return nil, err
			}
			kept = append(kept, next)
		}
	}
	return kept, nil
}

func (p *Path) matches(st step, item any, at string) (bool, error) {
	if st.matchKey == "" {
		return true, nil
	}
	obj, ok := item.(map[string]any)
	if !ok {
		return false, p.mismatch(at, "an object", item)
	}
	val, ok := obj[st.matchKey].(string)
	return ok && glob(st.pattern, val), nil
}

func (p *Path) mismatch(at, want string, got any) error {
	if at == "" {
		at = "the root"
	}
	return fmt.Errorf("remove %q: expected %s at %s, found %s", p.raw, want, at, kindOf(got))
}

func join(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

func kindOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	}
	return fmt.Sprintf("%T", v)
}

// glob matches s against a pattern in which "*" matches any run of characters.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// Apply removes paths from a JSON body: from each of its items when list is
// true, otherwise from the object itself. Numbers are kept exactly as sent.
func Apply(body []byte, paths []*Path, list bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if list {
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected a list object, found %s", kindOf(doc))
		}
		items, ok := obj["items"].([]any)
		if !ok && obj["items"] != nil {
			return nil, fmt.Errorf("expected items to be a list, found %s", kindOf(obj["items"]))
		}
		for i := range items {
			for _, p := range paths {
				out, err := p.Remove(items[i])
				if err != nil {
					return nil, fmt.Errorf("items[%d]: %w", i, err)
				}
				items[i] = out
			}
		}
	} else {
		for _, p := range paths {
			out, err := p.Remove(doc)
			if err != nil {
				return nil, err
			}
			doc = out
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Keep <, > and & as they were rather than \u-escaping them.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
