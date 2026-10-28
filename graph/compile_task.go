package graph

import (
	"fmt"
	"maps"
	"slices"
)

// task compiles one task and returns the flow after it. in is never
// modified.
func (c *compiler) task(parent string, t *taskAST, in flow) (*taskNode, flow) {
	path := parent + "/" + t.name
	n := &taskNode{path: path, timeout: t.timeout, optional: t.optional, background: t.background}
	c.tasks++
	if c.tasks > maxTasks {
		if c.tasks == maxTasks+1 {
			c.errf(path, t.pos, "document exceeds %d tasks after inlining subgraphs", maxTasks)
		}
		return n, in
	}
	if t.cond != "" {
		// Evaluated where the task starts, so it sees the flow before it.
		cond, err := c.conditionEnv(in).compile(t.cond)
		if err != nil {
			c.errf(path, t.condPos, "if: %v", err)
		}
		n.cond = cond
	}
	if t.background && t.optional {
		c.errf(path, t.pos, "background tasks cannot be optional: their failures never reach the run")
	}

	var out flow
	switch t.kind {
	case kindCall:
		n.op = t.call
		n.body, out = c.callTask(path, t, in.clone())
	case kindDo:
		n.body, out = c.region(path, t.wrap, t.do, in)
	case kindFork:
		n.body, out = c.forkTask(path, t, in)
	case kindSwitch:
		n.cases, out = c.switchTask(path, t, in)
	case kindRun:
		n.body, out = c.runTask(path, t, in)
	default: // a parse problem has been reported
		return n, in
	}
	switch {
	case t.background:
		// background work never publishes to the board.
		return n, in
	case t.optional, t.cond != "":
		// The task may fail (optional) or be skipped (if): what it writes may
		// be absent afterwards.
		out.demote(in)
	}
	return n, out
}

func (c *compiler) callTask(path string, t *taskAST, f flow) (plan, flow) {
	def, ok := c.opts.ops[t.call]
	if !ok {
		c.errf(path, t.pos, "unknown operator %q", t.call)
		return nil, f
	}
	cfg, err := c.config(t.with, def.in.configType())
	if err != nil {
		c.errf(path, t.pos, "operator %q config: %v", t.call, err)
	}
	c.checkRename(path, t, "in", t.in, def.in)
	c.checkRename(path, t, "out", t.out, def.out)
	where := fmt.Sprintf("%s (%s)", path, t.call)

	for _, fd := range def.in.fields {
		key := keyOf(t.in, fd.key)
		c.checkType(path, t.pos, key, fd.typ, where)
		c.checkRead(path, t.pos, fmt.Sprintf("operator %q", t.call), fd, key, f)
	}
	var mws []boundMw
	if len(c.opts.wrapCalls) > 0 { // read where the call starts, like the operator
		refs := make([]mwRef, len(c.opts.wrapCalls))
		for i, name := range c.opts.wrapCalls {
			refs[i] = mwRef{name: name, pos: t.pos}
		}
		mws = c.middlewares(path, refs, f)
	}

	written := make(map[string]string, len(def.out.fields)) // key -> port
	for _, fd := range def.out.fields {
		key := keyOf(t.out, fd.key)
		if port, dup := written[key]; dup {
			c.errf(path, t.pos, "out: ports %q and %q of operator %q both write %q", port, fd.key, t.call, key)
			continue
		}
		written[key] = fd.key
		c.checkType(path, t.pos, key, fd.typ, where)
		f.writes[key] = true
		f.set(key, fd.maybe)
	}
	call := &callNode{path: path, def: def, cfg: cfg, in: t.in, out: t.out}
	if mws == nil {
		return call, f
	}
	return &wrapNode{path: path, op: def.name, mws: mws, body: call}, f
}

func (c *compiler) checkRename(path string, t *taskAST, dir string, rename map[string]string, s shape) {
	for _, tag := range slices.Sorted(maps.Keys(rename)) {
		if !slices.ContainsFunc(s.fields, func(fd field) bool { return fd.key == tag }) {
			c.errf(path, t.pos, "%s: operator %q has no %s key %q", dir, t.call, dir, tag)
		}
	}
}

// forkTask: branches start from the same flow, isolated from each other. After
// the join a key is guaranteed if any branch guarantees it. Two branches may
// not write the same key: they run concurrently, so one write silently
// replacing the other is almost always a mistake.
func (c *compiler) forkTask(path string, t *taskAST, in flow) (plan, flow) {
	n := &forkNode{branches: make([]*taskNode, 0, len(t.branches))}
	out := in.clone()
	writer := make(map[string]string) // key -> first branch writing it
	for _, br := range t.branches {
		start := in.clone()
		start.writes = make(map[string]bool) // only this branch's writes
		bn, bf := c.task(path, br, start)
		n.branches = append(n.branches, bn)
		for _, k := range slices.Sorted(maps.Keys(bf.writes)) {
			if first, dup := writer[k]; dup {
				c.errf(path+"/"+br.name, br.pos, "fork branches %q and %q both write %q; write different keys and read both after the fork", first, br.name, k)
				continue
			}
			writer[k] = br.name
			out.writes[k] = true
		}
		out.include(bf)
	}
	return n, out
}

// switchTask: a key is guaranteed after the switch if every case guarantees
// it. A switch where no case matches fails, so "nothing ran" is
// not an outcome, with or without a default case.
func (c *compiler) switchTask(path string, t *taskAST, in flow) (*switchCases, flow) {
	n := &switchCases{path: path, cases: make([]switchCase, 0, len(t.cases))}
	conds := c.conditionEnv(in)
	outs := make([]flow, 0, len(t.cases))
	for _, cs := range t.cases {
		cpath := path + "/" + cs.name
		sc := switchCase{name: cs.name}
		if cs.name != defaultCase {
			cond, err := conds.compile(cs.when)
			if err != nil {
				c.errf(cpath, cs.whenPos, "when: %v", err)
			}
			sc.cond = cond
		}
		var cf flow
		sc.body, cf = c.seq(cpath, cs.do, in.clone())
		n.cases = append(n.cases, sc)
		outs = append(outs, cf)
	}
	if len(outs) == 0 { // an empty switch, already reported
		return n, in
	}
	return n, merge(outs)
}

// runTask inlines another graph at this point, sharing the caller's board.
// The subgraph is checked against the caller's flow. Unresolved and
// recursive references were reported by load.
func (c *compiler) runTask(path string, t *taskAST, in flow) (plan, flow) {
	g := t.target
	if g == nil || slices.Contains(c.stack, g) {
		return nil, in
	}
	return c.graphBody(g, path, in.clone())
}
