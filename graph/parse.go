package graph

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type parser struct {
	moduleID string // module id of the document being parsed
	problems []Problem
}

func (p *parser) at(n *yaml.Node) pos { return pos{p.moduleID, n.Line, n.Column} }

func (p *parser) errf(path string, at pos, format string, args ...any) {
	p.problems = append(p.problems, problemf(path, at, format, args...))
}

// parseDocument parses the document of module moduleID ("" for the root).
func parseDocument(moduleID string, src []byte) (*docAST, []Problem) {
	p := &parser{moduleID: moduleID}
	out := &docAST{}
	fail := func(msg string) (*docAST, []Problem) { return out, []Problem{{Module: moduleID, Msg: msg}} }
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return fail(err.Error())
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fail("empty document")
	}
	doc, ok := expandAliases(root.Content[0], maxYAMLNodes)
	if !ok {
		return fail(fmt.Sprintf("document exceeds %d YAML nodes after expanding aliases", maxYAMLNodes))
	}
	var hasGraphs, hasImport bool
	p.mapping("", doc, map[string]func(*yaml.Node){
		"graphs": func(n *yaml.Node) {
			hasGraphs = true
			if !p.expectKind("", n, yaml.MappingNode, "graphs must be a mapping of name to graph") {
				return
			}
			for i := 0; i < len(n.Content); i += 2 {
				out.graphs = append(out.graphs, p.graph(n.Content[i], n.Content[i+1]))
			}
		},
		"import": func(n *yaml.Node) {
			hasImport = true
			out.imports = p.imports(n)
		},
	})
	if !hasGraphs && !hasImport && doc.Kind == yaml.MappingNode {
		p.errf("", p.at(doc), "missing field %q", "graphs")
	}
	return out, p.problems
}

// imports parses `import: { alias: module id }`. A module id is opaque: it
// only has to match the id given to Import.
func (p *parser) imports(n *yaml.Node) []*importDecl {
	if !p.expectKind("", n, yaml.MappingNode, "import must be a mapping of alias to module id") {
		return nil
	}
	var out []*importDecl
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value == "" || strings.ContainsAny(k.Value, "./") {
			p.errf("", p.at(k), "invalid import alias %q (must be non-empty and contain no '.' or '/')", k.Value)
			continue
		}
		if id := p.str("", v); id != "" {
			out = append(out, &importDecl{alias: k.Value, id: id, pos: p.at(k)})
		}
	}
	return out
}

func (p *parser) graph(k, v *yaml.Node) *graphAST {
	g := &graphAST{moduleID: p.moduleID, name: k.Value, pos: p.at(k)}
	p.checkSegment(g.name, g.name, g.pos)
	if strings.Contains(g.name, ".") {
		p.errf(g.name, g.pos, "invalid graph name %q (must contain no '.', which separates an import alias from a graph name)", g.name)
	}
	p.mapping(g.name, v, map[string]func(*yaml.Node){
		"wrap": func(n *yaml.Node) { g.wrap = p.wrapList(g.name, n) },
		"do":   func(n *yaml.Node) { g.do = p.taskList(g.name, n) },
	}, "do")
	return g
}

func (p *parser) taskList(path string, n *yaml.Node) []*taskAST {
	if !p.expectKind(path, n, yaml.SequenceNode, "expected a list of tasks") {
		return nil
	}
	seen := map[string]bool{}
	var out []*taskAST
	for _, item := range n.Content {
		name, body, ok := p.singleKey(path, item, "a task is a single-key mapping `name: {...}`")
		if !ok {
			continue
		}
		if seen[name.Value] {
			p.errf(path, p.at(name), "duplicate task name %q", name.Value)
		}
		seen[name.Value] = true
		out = append(out, p.task(path, name, body))
	}
	return out
}

func (p *parser) task(parent string, k, v *yaml.Node) *taskAST {
	t := &taskAST{name: k.Value, pos: p.at(k)}
	path := parent + "/" + t.name
	p.checkSegment(path, t.name, t.pos)
	var kinds []string
	kind := func(name string, kd taskKind, f func(*yaml.Node)) func(*yaml.Node) {
		return func(n *yaml.Node) { kinds = append(kinds, name); t.kind = kd; f(n) }
	}
	p.mapping(path, v, map[string]func(*yaml.Node){
		"call": kind("call", kindCall, func(n *yaml.Node) { t.call = p.str(path, n) }),
		"do":   kind("do", kindDo, func(n *yaml.Node) { t.do = p.taskList(path, n) }),
		"fork": kind("fork", kindFork, func(n *yaml.Node) {
			if t.branches = p.taskList(path, n); len(t.branches) == 0 && n.Kind == yaml.SequenceNode {
				p.errf(path, p.at(n), "fork needs at least one branch")
			}
		}),
		"switch": kind("switch", kindSwitch, func(n *yaml.Node) {
			if t.cases = p.caseList(path, n); len(t.cases) == 0 && n.Kind == yaml.SequenceNode {
				p.errf(path, p.at(n), "switch needs at least one case")
			}
		}),
		"run":        kind("run", kindRun, func(n *yaml.Node) { t.graph = p.str(path, n) }),
		"wrap":       func(n *yaml.Node) { t.wrap = p.wrapList(path, n) },
		"with":       func(n *yaml.Node) { t.with = n },
		"in":         func(n *yaml.Node) { t.in = p.strMap(path, n) },
		"out":        func(n *yaml.Node) { t.out = p.strMap(path, n) },
		"timeout":    func(n *yaml.Node) { t.timeout = p.duration(path, n) },
		"optional":   func(n *yaml.Node) { t.optional = p.boolean(path, n) },
		"background": func(n *yaml.Node) { t.background = p.boolean(path, n) },
		"if":         func(n *yaml.Node) { t.cond, t.condPos = p.str(path, n), p.at(n) },
	})
	switch {
	case len(kinds) == 0:
		p.errf(path, t.pos, "task needs one of call, do, fork, switch, run")
	case len(kinds) > 1:
		p.errf(path, t.pos, "task has more than one of %s", strings.Join(kinds, ", "))
	}
	if t.kind != kindCall && (t.with != nil || t.in != nil || t.out != nil) {
		p.errf(path, t.pos, "with/in/out are only allowed on call tasks")
	}
	if t.wrap != nil && t.kind != kindDo {
		p.errf(path, t.pos, "wrap requires do")
	}
	return t
}

func (p *parser) caseList(path string, n *yaml.Node) []*caseAST {
	if !p.expectKind(path, n, yaml.SequenceNode, "switch expects a list of cases") {
		return nil
	}
	seen := map[string]bool{}
	var out []*caseAST
	for i, item := range n.Content {
		name, body, ok := p.singleKey(path, item, "a case is a single-key mapping `name: {when: ..., do: [...]}`, or `default: {do: [...]}`")
		if !ok {
			continue
		}
		c := &caseAST{name: name.Value, pos: p.at(name)}
		cpath := path + "/" + c.name
		p.checkSegment(cpath, c.name, c.pos)
		if seen[c.name] {
			p.errf(path, c.pos, "duplicate case name %q", c.name)
		}
		seen[c.name] = true
		p.mapping(cpath, body, map[string]func(*yaml.Node){
			"when": func(n *yaml.Node) { c.when, c.whenPos = p.str(cpath, n), p.at(n) },
			"do":   func(n *yaml.Node) { c.do = p.taskList(cpath, n) },
		}, "do")
		switch {
		case c.name == defaultCase && c.when != "":
			p.errf(cpath, c.whenPos, "the default case takes no when")
		case c.name == defaultCase && i != len(n.Content)-1:
			p.errf(cpath, c.pos, "the default case must be last")
		case c.name != defaultCase && c.when == "":
			p.errf(cpath, c.pos, "case %q needs a when (only the case named default has none)", c.name)
		}
		out = append(out, c)
	}
	return out
}

func (p *parser) wrapList(path string, n *yaml.Node) []mwRef {
	if !p.expectKind(path, n, yaml.SequenceNode, "wrap expects a list of middleware") {
		return nil
	}
	var out []mwRef
	for _, item := range n.Content {
		if item.Kind == yaml.ScalarNode {
			out = append(out, mwRef{name: item.Value, pos: p.at(item)})
			continue
		}
		name, cfg, ok := p.singleKey(path, item, "a wrap entry is `name` or `name: {config}`")
		if ok {
			out = append(out, mwRef{name: name.Value, pos: p.at(name), cfg: cfg})
		}
	}
	return out
}
