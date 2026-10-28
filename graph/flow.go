package graph

import (
	"maps"
	"slices"
)

// flow is the dataflow state at one point of a graph: keys guaranteed to be
// set (def), keys that may be set (maybe), and keys written by some task on
// the way here (writes; the board as seeded by the entry input is not a
// write). writes is how a fork finds branches that write the same key.
type flow struct {
	def, maybe, writes map[string]bool
}

func newFlow() flow {
	return flow{def: make(map[string]bool), maybe: make(map[string]bool), writes: make(map[string]bool)}
}

func (f flow) clone() flow {
	return flow{def: maps.Clone(f.def), maybe: maps.Clone(f.maybe), writes: maps.Clone(f.writes)}
}

func (f flow) setDef(k string) {
	f.def[k] = true
	delete(f.maybe, k)
}

func (f flow) setMaybe(k string) {
	if !f.def[k] {
		f.maybe[k] = true
	}
}

// set records a write of k: guaranteed, or only possible when maybe.
func (f flow) set(k string, maybe bool) {
	if maybe {
		f.setMaybe(k)
	} else {
		f.setDef(k)
	}
}

// include adds what one fork branch guarantees and may write: branches all
// run, so a key any of them guarantees is guaranteed after the fork.
func (f flow) include(branch flow) {
	for k := range branch.def {
		f.setDef(k)
	}
	for k := range branch.maybe {
		f.setMaybe(k)
	}
}

// demote turns keys that became guaranteed since before into maybe: the work
// that wrote them may be lost.
func (f flow) demote(before flow) {
	for k := range f.def {
		if !before.def[k] {
			delete(f.def, k)
			f.maybe[k] = true
		}
	}
}

// merge joins the flows of alternative paths. outs is never empty.
func merge(outs []flow) flow {
	out := newFlow()
	for k := range outs[0].def {
		if !slices.ContainsFunc(outs[1:], func(f flow) bool { return !f.def[k] }) {
			out.def[k] = true
		}
	}
	for _, f := range outs {
		maps.Copy(out.writes, f.writes)
		for k := range f.def {
			out.setMaybe(k)
		}
		for k := range f.maybe {
			out.setMaybe(k)
		}
	}
	return out
}
