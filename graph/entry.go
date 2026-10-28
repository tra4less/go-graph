package graph

import (
	"reflect"
)

// Entry declares a graph that callers can run, with typed input and output.
// It only names the graph and its types; Run runs it on a Program.
//
// In and Out follow the same rules as an operator's: structs whose
// `graph:"key"` fields name blackboard keys. In seeds the blackboard: its
// non-Maybe fields are guaranteed to be set throughout the graph, its Maybe
// fields only when OK. Out is read from the final blackboard: Compile
// checks that every non-Maybe field is written on every path through the
// graph, and that every Maybe field is written on some path.
//
// Declare an Entry once, typically as a package-level variable, and pass it
// to Compile with the operators (an *Entry is an Option):
//
//	var Search = graph.NewEntry[Request, Response]("search")
//
//	prog, err := graph.Compile(doc, Search, graph.Op(...), ...)
//	out, err := graph.Run(ctx, prog, Search, Request{...})
//
// Graphs without an Entry are subgraphs: they cannot be run directly, only
// inlined with run.
//
// The zero Entry has no graph name: registering it is an error and running
// it returns an error.
type Entry[In, Out any] struct {
	graph string
}

// entryDef is an Entry as given to Compile. key is the *Entry itself:
// Programs look plans up by it, so two Entries for the same graph name are
// never confused.
type entryDef struct {
	key     any
	in, out shape
}

// NewEntry declares an entry for the graph named name in the root
// document: a graph of the root document itself, or alias.graph for a
// public graph of a module the root document imports (see Import). It does
// no validation; Compile checks the name and the In and Out types.
func NewEntry[In, Out any](name string) *Entry[In, Out] {
	return &Entry[In, Out]{graph: name}
}

func (e *Entry[In, Out]) apply(o *options) {
	switch {
	case e == nil || e.graph == "":
		o.fail("entry without a graph name (use NewEntry)")
		return
	case o.inOverride:
		o.fail("Override cannot replace entries")
		return
	case o.entries[e.graph] != nil:
		o.fail("entry for graph %q registered twice", e.graph)
		return
	}
	in, err := analyzeShape(reflect.TypeFor[In](), dataOnly)
	if err != nil {
		o.fail("entry %q input: %w", e.graph, err)
		return
	}
	out, err := analyzeShape(reflect.TypeFor[Out](), dataOnly)
	if err != nil {
		o.fail("entry %q output: %w", e.graph, err)
		return
	}
	o.entries[e.graph] = &entryDef{key: e, in: in, out: out}
}
