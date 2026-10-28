package graph

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// mapping walks a mapping node, dispatching known keys to handlers and
// reporting unknown, duplicate and missing required keys.
func (p *parser) mapping(path string, n *yaml.Node, handlers map[string]func(*yaml.Node), required ...string) {
	if !p.expectKind(path, n, yaml.MappingNode, "expected a mapping") {
		return
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		h, ok := handlers[k.Value]
		switch {
		case !ok:
			p.errf(path, p.at(k), "unknown field %q", k.Value)
		case seen[k.Value]:
			p.errf(path, p.at(k), "duplicate field %q", k.Value)
		default:
			seen[k.Value] = true
			h(v)
		}
	}
	for _, r := range required {
		if !seen[r] {
			p.errf(path, p.at(n), "missing field %q", r)
		}
	}
}

func (p *parser) expectKind(path string, n *yaml.Node, kind yaml.Kind, msg string) bool {
	if n.Kind != kind {
		p.errf(path, p.at(n), "%s", msg)
		return false
	}
	return true
}

func (p *parser) singleKey(path string, n *yaml.Node, msg string) (k, v *yaml.Node, ok bool) {
	if n.Kind != yaml.MappingNode || len(n.Content) != 2 {
		p.errf(path, p.at(n), "%s", msg)
		return nil, nil, false
	}
	return n.Content[0], n.Content[1], true
}

func (p *parser) checkSegment(path, name string, at pos) {
	if name == "" || strings.Contains(name, "/") {
		p.errf(path, at, "invalid name %q (must be non-empty and contain no '/')", name)
	}
}

func (p *parser) str(path string, n *yaml.Node) string {
	if n.Kind != yaml.ScalarNode || n.Value == "" {
		p.errf(path, p.at(n), "expected a non-empty string")
		return ""
	}
	return n.Value
}

func (p *parser) strMap(path string, n *yaml.Node) map[string]string {
	if !p.expectKind(path, n, yaml.MappingNode, "expected a mapping of strings") {
		return nil
	}
	out := map[string]string{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if _, dup := out[k.Value]; dup {
			p.errf(path, p.at(k), "duplicate field %q", k.Value)
		}
		out[k.Value] = p.str(path, v)
	}
	return out
}

func (p *parser) duration(path string, n *yaml.Node) time.Duration {
	d, err := time.ParseDuration(n.Value)
	if n.Kind != yaml.ScalarNode || err != nil || d <= 0 {
		p.errf(path, p.at(n), "timeout must be a positive duration like 200ms, got %q", n.Value)
		return 0
	}
	return d
}

func (p *parser) boolean(path string, n *yaml.Node) bool {
	var b bool
	if n.Kind != yaml.ScalarNode || n.Decode(&b) != nil {
		p.errf(path, p.at(n), "expected true or false, got %q", n.Value)
	}
	return b
}

// maxYAMLNodes bounds alias expansion ("billion laughs").
const maxYAMLNodes = 100_000

// expandAliases returns a copy of n with every alias replaced by a copy of
// its anchored node and anchors cleared, so later stages (and the strict
// config decoder, which re-marshals subtrees) never see an alias. It reports
// false if the expanded tree would exceed budget nodes; this also stops
// self-referencing aliases.
func expandAliases(n *yaml.Node, budget int) (*yaml.Node, bool) {
	var expand func(*yaml.Node) *yaml.Node
	expand = func(n *yaml.Node) *yaml.Node {
		if budget--; budget < 0 {
			return nil
		}
		if n.Kind == yaml.AliasNode {
			return expand(n.Alias)
		}
		c := *n
		c.Anchor = ""
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			if c.Content[i] = expand(child); c.Content[i] == nil {
				return nil
			}
		}
		return &c
	}
	out := expand(n)
	return out, out != nil
}
