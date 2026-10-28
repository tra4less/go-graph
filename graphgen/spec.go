package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/token"
	"io"
	"strings"

	"golang.org/x/mod/module"
	"gopkg.in/yaml.v3"
)

// spec is a parsed operators.yaml.
type spec struct {
	file       string // for error positions
	operators  []decl
	middleware []decl
	entries    []decl
}

// decl declares one component: a graph name (empty for entries) bound to a
// Go symbol.
type decl struct {
	name string
	sym  symbol
	line int
	col  int
}

// symbol is a reference to a package-level function, a method or a variable.
type symbol struct {
	pkg  string // full import path
	recv string // receiver type name for methods; empty otherwise
	ptr  bool   // pointer receiver
	name string // function, method or variable name
}

func (s symbol) String() string {
	var b strings.Builder
	b.WriteString(s.pkg)
	b.WriteString(".")
	switch {
	case s.recv != "" && s.ptr:
		fmt.Fprintf(&b, "(*%s).", s.recv)
	case s.recv != "":
		fmt.Fprintf(&b, "%s.", s.recv)
	}
	b.WriteString(s.name)
	return b.String()
}

// specError is a problem in operators.yaml, with its position.
type specError struct {
	file      string
	line, col int
	msg       string
}

func (e *specError) Error() string {
	if e.line > 0 {
		return fmt.Sprintf("%s:%d:%d: %s", e.file, e.line, e.col, e.msg)
	}
	return fmt.Sprintf("%s: %s", e.file, e.msg)
}

// errorList collects problems so that one run reports all of them.
type errorList []error

func (l *errorList) add(file string, line, col int, format string, args ...any) {
	*l = append(*l, &specError{file: file, line: line, col: col, msg: fmt.Sprintf(format, args...)})
}

func (l errorList) err() error { return errors.Join(l...) }

type rawDecl struct {
	Package string `yaml:"package"`
	Func    string `yaml:"func"`
	Var     string `yaml:"var"`
}

// parseSpec parses operators.yaml. Unknown fields, duplicate names and
// malformed symbols are errors; all of them are reported together.
func parseSpec(file string, src []byte) (*spec, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	s := &spec{file: file}
	var errs errorList
	if len(doc.Content) == 0 {
		errs.add(file, 0, 0, "empty document")
		return nil, errs.err()
	}
	root, ok := expandAliases(doc.Content[0], maxNodes)
	if !ok {
		errs.add(file, 0, 0, "document exceeds %d YAML nodes after expanding aliases", maxNodes)
		return nil, errs.err()
	}
	if root.Kind != yaml.MappingNode {
		errs.add(file, root.Line, root.Column, "expected a mapping with operators, middleware, entries")
		return nil, errs.err()
	}
	for i := 0; i < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch {
		case strings.HasPrefix(k.Value, "x-"):
			// Extension fields are ignored; they hold YAML anchors.
		case k.Value == "operators":
			s.operators = parseDecls(file, v, funcField, &errs)
		case k.Value == "middleware":
			s.middleware = parseDecls(file, v, funcField, &errs)
		case k.Value == "entries":
			s.entries = parseEntries(file, v, &errs)
		default:
			errs.add(file, k.Line, k.Column, "unknown field %q (want operators, middleware, entries, or x-* for anchors)", k.Value)
		}
	}
	if len(s.operators)+len(s.middleware)+len(s.entries) == 0 {
		errs.add(file, root.Line, root.Column, "nothing declared")
	}
	if len(errs) > 0 {
		return nil, errs.err()
	}
	return s, nil
}

// symField is the key of a declaration that names its Go symbol.
type symField string

const (
	funcField symField = "func" // operators and middleware: a function or method
	varField  symField = "var"  // entries: a package-level variable
)

// parseDecls parses a mapping of graph name to {package, func}.
func parseDecls(file string, n *yaml.Node, symKey symField, errs *errorList) []decl {
	if n.Kind != yaml.MappingNode {
		errs.add(file, n.Line, n.Column, "expected a mapping of name to {package, %s}", symKey)
		return nil
	}
	var out []decl
	seen := make(map[string]bool)
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value == "" {
			errs.add(file, k.Line, k.Column, "empty name")
			continue
		}
		if seen[k.Value] {
			errs.add(file, k.Line, k.Column, "%q declared twice", k.Value)
			continue
		}
		seen[k.Value] = true
		sym, ok := parseRaw(file, v, symKey, errs)
		if ok {
			out = append(out, decl{name: k.Value, sym: sym, line: k.Line, col: k.Column})
		}
	}
	return out
}

// parseEntries parses a list of {package, var}.
func parseEntries(file string, n *yaml.Node, errs *errorList) []decl {
	if n.Kind != yaml.SequenceNode {
		errs.add(file, n.Line, n.Column, "entries must be a list of {package, var}")
		return nil
	}
	var out []decl
	for _, item := range n.Content {
		if sym, ok := parseRaw(file, item, varField, errs); ok {
			out = append(out, decl{sym: sym, line: item.Line, col: item.Column})
		}
	}
	return out
}

func parseRaw(file string, n *yaml.Node, symKey symField, errs *errorList) (symbol, bool) {
	var raw rawDecl
	if err := strictDecode(n, &raw); err != nil {
		errs.add(file, n.Line, n.Column, "%v", err)
		return symbol{}, false
	}
	ref := raw.Func
	other := raw.Var
	if symKey == varField {
		ref, other = raw.Var, raw.Func
	}
	if other != "" {
		errs.add(file, n.Line, n.Column, "use %q here, not the other one of func/var", symKey)
		return symbol{}, false
	}
	if ref == "" {
		errs.add(file, n.Line, n.Column, "missing %q", symKey)
		return symbol{}, false
	}
	if raw.Package == "" {
		errs.add(file, n.Line, n.Column, `missing "package" (the full import path, e.g. github.com/tra4less/go-graph/examples/search)`)
		return symbol{}, false
	}
	if err := module.CheckImportPath(raw.Package); err != nil {
		errs.add(file, n.Line, n.Column, "package must be a full import path: %v", err)
		return symbol{}, false
	}
	sym, err := parseSymbol(ref, symKey == funcField)
	if err != nil {
		errs.add(file, n.Line, n.Column, "%v", err)
		return symbol{}, false
	}
	sym.pkg = raw.Package
	return sym, true
}

// parseSymbol parses Name, (*Type).Method or Type.Method.
func parseSymbol(ref string, methods bool) (symbol, error) {
	bad := fmt.Errorf("bad symbol %q (want Name, (*Type).Method or Type.Method)", ref)
	recv, name, isMethod := strings.Cut(ref, ".")
	if !isMethod {
		if !token.IsIdentifier(ref) {
			return symbol{}, bad
		}
		return symbol{name: ref}, nil
	}
	if !methods {
		return symbol{}, fmt.Errorf("bad symbol %q: entries are package-level variables", ref)
	}
	if !token.IsIdentifier(name) {
		return symbol{}, bad
	}
	s := symbol{name: name}
	if t, ok := strings.CutPrefix(recv, "(*"); ok {
		t, ok = strings.CutSuffix(t, ")")
		if !ok {
			return symbol{}, bad
		}
		s.recv, s.ptr = t, true
	} else {
		s.recv = recv
	}
	if !token.IsIdentifier(s.recv) {
		return symbol{}, bad
	}
	return s, nil
}

func strictDecode(n *yaml.Node, v any) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("expected a mapping like {package: ..., func: ...}")
	}
	buf, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// maxNodes bounds alias expansion ("billion laughs").
const maxNodes = 100_000

// expandAliases returns a copy of n with every alias replaced by a copy of its
// anchored node, so that subtrees can be decoded on their own. It reports
// false if the result would exceed budget nodes, which also stops
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
