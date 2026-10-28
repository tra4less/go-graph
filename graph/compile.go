package graph

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// maxTasks bounds the size of a compiled document after subgraphs are
// inlined, so a hot-reloaded document cannot make Compile blow up
// exponentially (A runs B twice, B runs C twice, ...).
const maxTasks = 10_000

// Compile checks opts, then parses and checks the root graph document src,
// and every document it imports (see Import), against them, and returns an
// immutable, concurrency-safe Program.
//
// Errors come in two kinds, so that hot reload can tell a bad document from
// a bad build:
//
//   - a mistake in opts (a malformed graph tag, a duplicate name, a bad
//     Override, ...) is a plain error, reported before any document is
//     looked at;
//   - a problem in a document is a *CompileError listing every problem
//     found, with YAML positions; Problem.Module names the imported
//     document a problem is in.
//
// Compile is pure: no side effects, no global state, no I/O. Hot reload is
// the caller's job: keep the code options (operators, middleware, entries,
// ...), build the Import options from the new documents, Compile and, on
// success, swap the result into an atomic.Pointer[Program]; on failure keep
// serving the old one. All documents of a Program change together.
func Compile(src []byte, opts ...Option) (*Program, error) {
	o, err := buildOptions(opts)
	if err != nil {
		return nil, err
	}
	comp := &compiler{
		opts:    o,
		envs:    make(map[string]*conditionEnv),
		typeIDs: make(map[reflect.Type]int),
		cfgs:    make(map[configKey]decodedConfig),
	}
	comp.load(src)
	root := comp.modules[0]

	p := &Program{entries: make(map[any]*entryPlan, len(o.entries))}
	var entryGraphs []*graphAST
	for _, name := range slices.Sorted(maps.Keys(o.entries)) {
		g := comp.entryGraph(root, name)
		if g == nil {
			continue
		}
		e := o.entries[name]
		entryGraphs = append(entryGraphs, g)
		p.entries[e.key] = comp.entry(g, name, e)
	}
	comp.checkUnused(entryGraphs)
	if len(comp.problems) > 0 {
		return nil, &CompileError{Problems: dedupe(comp.problems)}
	}
	return p, nil
}

// dedupe drops repeats of the same finding, which arise when a subgraph is
// inlined at several places.
func dedupe(ps []Problem) []Problem {
	type key struct {
		mod       string
		line, col int
		msg       string
	}
	seen := make(map[key]bool, len(ps))
	out := ps[:0]
	for _, p := range ps {
		k := key{p.Module, p.Line, p.Column, p.Msg}
		if p.Line > 0 && seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

type keyType struct {
	typ   reflect.Type
	where string
}

// compiler holds the state of one Compile. Everything is Program-wide
// except types, which belongs to the entry being compiled.
type compiler struct {
	opts     *options
	modules  []*module // the root document first, then imports in load order
	tasks    int       // tasks compiled so far, see maxTasks
	problems []Problem

	// Work that does not depend on where a subgraph is inlined is done
	// once: config decoding per YAML node, conditions per environment.
	cfgs    map[configKey]decodedConfig
	envs    map[string]*conditionEnv // by signature, see conditionEnv
	typeIDs map[reflect.Type]int

	// Per entry.
	types map[string]keyType // Go type bound to each key name
	stack []*graphAST        // graphs being inlined
}

func (c *compiler) errf(path string, at pos, format string, args ...any) {
	c.problems = append(c.problems, problemf(path, at, format, args...))
}

// entry compiles g as the graph of entry e, named name in the root
// document: e's input seeds the flow, and e's output must be produced by the
// end of the graph.
func (c *compiler) entry(g *graphAST, name string, e *entryDef) *entryPlan {
	// Unrelated entries may use the same key name for different types.
	c.types = make(map[string]keyType)
	inWhere := fmt.Sprintf("Entry %q input", name)
	f := newFlow()
	for _, fd := range e.in.fields {
		c.checkType(name, g.pos, fd.key, fd.typ, inWhere)
		f.set(fd.key, fd.maybe)
	}
	body, out := c.graphBody(g, name, f)
	outWhere := fmt.Sprintf("Entry %q output", name)
	for _, fd := range e.out.fields {
		switch {
		case out.def[fd.key], fd.maybe && out.maybe[fd.key]:
			c.checkType(name, g.pos, fd.key, fd.typ, outWhere)
		case out.maybe[fd.key]:
			c.errf(name, g.pos, "Entry output %q may be absent when the graph ends; declare the field as graph.Maybe", fd.key)
		default:
			c.errf(name, g.pos, "Entry output %q is never written by the graph", fd.key)
		}
	}
	return &entryPlan{in: e.in, out: e.out, body: body}
}

// graphBody compiles g's wrap and do under path, inlined into flow in.
func (c *compiler) graphBody(g *graphAST, path string, in flow) (plan, flow) {
	c.stack = append(c.stack, g)
	defer func() { c.stack = c.stack[:len(c.stack)-1] }()
	return c.region(path, g.wrap, g.do, in)
}

// region compiles tasks in sequence under path, inside the middleware refs
// when there are any. Middleware In is checked against in, the flow where
// the region starts.
func (c *compiler) region(path string, refs []mwRef, tasks []*taskAST, in flow) (plan, flow) {
	var mws []boundMw
	if len(refs) > 0 {
		mws = c.middlewares(path, refs, in)
	}
	body, out := c.seq(path, tasks, in)
	if len(refs) == 0 {
		return body, out
	}
	return &wrapNode{path: path, mws: mws, body: body}, out
}

func (c *compiler) seq(path string, tasks []*taskAST, f flow) (*seqNode, flow) {
	n := &seqNode{tasks: make([]*taskNode, 0, len(tasks))}
	for _, t := range tasks {
		var tn *taskNode
		tn, f = c.task(path, t, f)
		n.tasks = append(n.tasks, tn)
	}
	return n, f
}

// middlewares binds the wrap list of a region whose entry flow is f.
func (c *compiler) middlewares(path string, refs []mwRef, f flow) []boundMw {
	out := make([]boundMw, 0, len(refs))
	for _, r := range refs {
		def, ok := c.opts.mws[r.name]
		if !ok {
			c.errf(path, r.pos, "unknown middleware %q", r.name)
			continue
		}
		where := fmt.Sprintf("%s (middleware %s)", path, r.name)
		for _, fd := range def.in.fields {
			c.checkType(path, r.pos, fd.key, fd.typ, where)
			c.checkRead(path, r.pos, fmt.Sprintf("middleware %q", r.name), fd, fd.key, f)
		}
		cfg, err := c.config(r.cfg, def.in.configType())
		if err != nil {
			c.errf(path, r.pos, "middleware %q config: %v", r.name, err)
			continue
		}
		out = append(out, boundMw{def: def, cfg: cfg})
	}
	return out
}

// checkRead reports a read of key that is not guaranteed at flow f, unless
// the field is a Maybe and the key may be set.
func (c *compiler) checkRead(path string, at pos, who string, fd field, key string, f flow) {
	switch {
	case f.def[key], fd.maybe && f.maybe[key]:
	case f.maybe[key]:
		c.errf(path, at, "%s reads %q, which may be absent here (written by an optional task, a task with if, some switch cases, or a Maybe entry input); read it as graph.Maybe", who, key)
	default:
		c.errf(path, at, "%s reads %q, but nothing before this point writes it", who, key)
	}
}

// checkType records or verifies the Go type bound to a key name.
func (c *compiler) checkType(path string, at pos, key string, t reflect.Type, where string) {
	if d, ok := c.types[key]; ok {
		if d.typ != t {
			c.errf(path, at, "key %q is %v here but %v at %s", key, t, d.typ, d.where)
		}
		return
	}
	c.types[key] = keyType{typ: t, where: where}
}
