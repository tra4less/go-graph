package graph

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// A module is one graph document: the one given to Compile (the root, id "")
// or one given by Import. Graph names are scoped to their module; a document
// reaches another module's graphs as alias.graph, through its import: list.

type module struct {
	id      string // "" for the root document
	graphs  map[string]*graphAST
	order   []*graphAST            // declaration order
	imports map[string]*importDecl // by alias
	deps    []*importDecl          // declaration order
}

type importDecl struct {
	alias, id string
	pos       pos
	mod       *module // nil when the import is not provided
	used      bool    // some run task or Entry names alias.graph
}

// errNotProvided: a name refers to a module whose Import is missing. The
// missing import is reported where it is declared, so lookups that hit it
// report nothing more.
var errNotProvided = errors.New("module not provided")

// isPrivate reports whether a graph name is visible only in its own document.
func isPrivate(name string) bool { return strings.HasPrefix(name, "_") }

// load parses the root document and, transitively, every document it
// imports, into c.modules (the root first), then resolves every run task to
// its graph. It reports missing and cyclic imports and recursive graph
// references, so that compiling (which inlines along the resolved
// references) can assume none.
func (c *compiler) load(src []byte) {
	byID := make(map[string]*module)
	root := c.parseModule("", src)
	c.modules = []*module{root}
	for i := 0; i < len(c.modules); i++ { // c.modules grows: breadth-first
		for _, d := range c.modules[i].deps {
			if m, ok := byID[d.id]; ok {
				d.mod = m
				continue
			}
			data, ok := c.opts.imports[d.id]
			if !ok {
				c.errf("", d.pos, "import %q is not provided; pass graph.Import(%q, ...) to Compile", d.id, d.id)
				continue
			}
			d.mod = c.parseModule(d.id, data)
			byID[d.id] = d.mod
			c.modules = append(c.modules, d.mod)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(c.opts.imports)) {
		if byID[id] == nil {
			c.errf("", pos{moduleID: id}, "graph.Import %q is given, but no document imports it", id)
		}
	}
	c.checkImportCycles()
	for _, m := range c.modules {
		for _, g := range m.order {
			eachRun(g.name, g.do, func(path string, t *taskAST) {
				t.path = path
				t.target = c.resolveRun(m, t)
				if t.target != nil {
					g.calls = append(g.calls, t)
				}
			})
		}
	}
	c.checkRecursion()
}

// parseModule parses one document into a module, reporting duplicate graph
// names and import aliases.
func (c *compiler) parseModule(id string, src []byte) *module {
	doc, problems := parseDocument(id, src)
	c.problems = append(c.problems, problems...)
	m := &module{id: id, graphs: make(map[string]*graphAST), imports: make(map[string]*importDecl)}
	byID := make(map[string]*importDecl)
	for _, d := range doc.imports {
		switch first := byID[d.id]; {
		case m.imports[d.alias] != nil:
			c.errf("", d.pos, "duplicate import alias %q", d.alias)
		case first != nil:
			c.errf("", d.pos, "module %q is imported twice (as %q and %q)", d.id, first.alias, d.alias)
		default:
			m.imports[d.alias] = d
			byID[d.id] = d
			m.deps = append(m.deps, d)
		}
	}
	for _, g := range doc.graphs {
		if _, dup := m.graphs[g.name]; dup {
			c.errf(g.name, g.pos, "duplicate graph %q", g.name)
			continue
		}
		m.graphs[g.name] = g
		m.order = append(m.order, g)
	}
	return m
}

// lookup returns the graph that name denotes in m: a graph of m, or
// alias.graph for a public graph of a module m imports. It marks the alias
// as used, so that unused imports can be reported. The error is
// errNotProvided when the alias's module is missing.
func (m *module) lookup(name string) (*graphAST, error) {
	alias, local, qualified := strings.Cut(name, ".")
	if !qualified {
		if g := m.graphs[name]; g != nil {
			return g, nil
		}
		return nil, fmt.Errorf("unknown graph %q", name)
	}
	d := m.imports[alias]
	if d == nil {
		return nil, fmt.Errorf("the document imports nothing as %q (see import:)", alias)
	}
	d.used = true
	if d.mod == nil {
		return nil, errNotProvided
	}
	g := d.mod.graphs[local]
	switch {
	case g == nil:
		return nil, fmt.Errorf("module %q has no graph %q", d.id, local)
	case isPrivate(local):
		return nil, fmt.Errorf("graph %q of module %q is private (names starting with _ are visible only in their own document)", local, d.id)
	}
	return g, nil
}

// resolveRun returns the graph run task t of module m runs, or nil after
// reporting why there is none.
func (c *compiler) resolveRun(m *module, t *taskAST) *graphAST {
	g, err := m.lookup(t.graph)
	switch {
	case err == nil:
		return g
	case errors.Is(err, errNotProvided):
	case strings.Contains(t.graph, "."):
		c.errf(t.path, t.pos, "run %q: %v", t.graph, err)
	default:
		c.errf(t.path, t.pos, "%v", err)
	}
	return nil
}

// entryGraph finds the graph of the Entry for name, in the root document's
// scope. Entries are outside callers, so they only reach public graphs.
func (c *compiler) entryGraph(root *module, name string) *graphAST {
	g, err := root.lookup(name)
	if err == nil && isPrivate(g.name) {
		err = fmt.Errorf("graph %q is private (names starting with _ can only be run by their own document)", name)
	}
	switch {
	case err == nil:
		return g
	case !errors.Is(err, errNotProvided):
		c.errf(name, pos{}, "Entry %q: %v", name, err)
	}
	return nil
}

// checkUnused reports graphs no Entry and no run can reach, and import
// aliases nothing uses. In the root document every graph must be reachable
// from an Entry. An imported document is a library: its public graphs may go
// unused by this Program, but its private ones must be run by the document.
func (c *compiler) checkUnused(entries []*graphAST) {
	reached := make(map[*graphAST]bool)
	var visit func(*graphAST)
	visit = func(g *graphAST) {
		if reached[g] {
			return
		}
		reached[g] = true
		for _, t := range g.calls {
			visit(t.target)
		}
	}
	for _, g := range entries {
		visit(g)
	}
	for _, m := range c.modules {
		runLocally := make(map[*graphAST]bool)
		for _, g := range m.order {
			for _, t := range g.calls {
				runLocally[t.target] = true
			}
		}
		for _, g := range m.order {
			switch {
			case m.id == "" && !reached[g] && runLocally[g]:
				c.errf(g.name, g.pos, "graph %q is run only by graphs no Entry reaches", g.name)
			case m.id == "" && !reached[g]:
				c.errf(g.name, g.pos, "graph %q has no Entry and no graph runs it", g.name)
			case m.id != "" && isPrivate(g.name) && !runLocally[g]:
				c.errf(g.name, g.pos, "private graph %q is never run by its document", g.name)
			}
		}
		for _, d := range m.deps {
			if !d.used {
				c.errf("", d.pos, "import %q (%s) is never used", d.alias, d.id)
			}
		}
	}
}

// checkImportCycles reports a document that imports itself, directly or
// not. Cycles are harmless to inlining, but like Go's they always mean the
// modules should be split differently.
func (c *compiler) checkImportCycles() {
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[*module]int)
	var stack []string
	var visit func(*module)
	visit = func(m *module) {
		state[m] = visiting
		stack = append(stack, displayID(m.id))
		for _, d := range m.deps {
			switch {
			case d.mod == nil || state[d.mod] == done:
			case state[d.mod] == visiting:
				i := slices.Index(stack, displayID(d.mod.id))
				c.errf("", d.pos, "import cycle: %s -> %s", strings.Join(stack[i:], " -> "), displayID(d.mod.id))
			default:
				visit(d.mod)
			}
		}
		stack = stack[:len(stack)-1]
		state[m] = done
	}
	for _, m := range c.modules {
		if state[m] == 0 {
			visit(m)
		}
	}
}

func displayID(id string) string {
	if id == "" {
		return "(root)"
	}
	return id
}

// checkRecursion reports run references that form a cycle: inlining them
// would never end.
func (c *compiler) checkRecursion() {
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[*graphAST]int)
	var stack []*graphAST
	var visit func(*graphAST)
	visit = func(g *graphAST) {
		state[g] = visiting
		stack = append(stack, g)
		for _, t := range g.calls {
			switch state[t.target] {
			case visiting:
				i := slices.Index(stack, t.target)
				names := make([]string, 0, len(stack)-i+1)
				for _, s := range stack[i:] {
					names = append(names, s.qualified())
				}
				names = append(names, t.target.qualified())
				c.errf(t.path, t.pos, "recursive graph reference: %s", strings.Join(names, " -> "))
			case 0:
				visit(t.target)
			}
		}
		stack = stack[:len(stack)-1]
		state[g] = done
	}
	for _, m := range c.modules {
		for _, g := range m.order {
			if state[g] == 0 {
				visit(g)
			}
		}
	}
}

// eachRun calls f for every run task under tasks, with its path.
func eachRun(path string, tasks []*taskAST, f func(path string, t *taskAST)) {
	for _, t := range tasks {
		p := path + "/" + t.name
		if t.kind == kindRun {
			f(p, t)
		}
		eachRun(p, t.do, f)
		eachRun(p, t.branches, f)
		for _, cs := range t.cases {
			eachRun(p+"/"+cs.name, cs.do, f)
		}
	}
}
