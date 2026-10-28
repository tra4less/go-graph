// Package graph is an in-process operator scheduler: operators are plain Go
// functions, topology is a YAML document, Compile checks the document against
// the operators (passed as options) and returns an immutable Program, and an
// Entry runs one graph of the Program on a fresh blackboard.
//
// # Data
//
// Operators, entries and middleware exchange data only through the
// blackboard of a run, addressed by key name. An operator is
// func(ctx, In) (Out, error). In and Out are structs that declare what it
// reads and writes:
//
//	type SearchIn struct {
//		Cfg   SearchCfg         `graph:",with"`  // the node's `with:` config
//		UID   string            `graph:"uid"`    // must be set before this task
//		Query *Query            `graph:"parsed"` // written by any earlier task
//		Hint  graph.Maybe[string] `graph:"hint"`   // may be absent
//	}
//	type SearchOut struct {
//		Docs []Doc `graph:"docs"`
//	}
//
// Every exported field carries `graph:"key"`; struct{} binds nothing. An
// operator's In and a middleware's V may have one `graph:",with"` field,
// which receives the node's config instead of reading the blackboard. An
// Maybe[T] input field reads a key that may be absent; a Maybe[T] output field
// is written only when OK. A key name has one Go type within an entry's graph
// (subgraphs included); unrelated entries may differ.
//
// Callers run a graph through an Entry, whose In and Out follow the same
// rules:
//
//	var Search = graph.NewEntry[Request, Response]("search")
//	out, err := graph.Run(ctx, prog, Search, Request{UID: "u1", Query: "go"})
//
// The library records nothing about a run: no trace, no metrics. To observe
// one, use middleware; WrapCalls puts it around every operator.
//
// # Documents
//
//	graphs:
//	  search:                          # has an Entry: runnable
//	    do:
//	      - parse:  { call: parse_query }
//	      - recall:
//	          fork:
//	            - title:  { call: recall, with: { source: title }, out: { docs: title_docs } }
//	            - vector: { call: recall, with: { source: vector }, out: { docs: vec_docs },
//	                        timeout: 50ms, optional: true }
//	            - profile: { call: load_profile }
//	      - merge:  { call: merge, in: { primary: title_docs, extra: vec_docs } }
//	      - rerank: { call: rerank, if: "size(docs) > 3" }
//	      - rank:
//	          switch:
//	            - personal: { when: "profile.HasHistory", do: [ { p: { call: personalize } } ] }
//	            - default:  { do: [ { d: { run: default_rank } } ] }
//	      - detail:
//	          wrap: [ { retry: { attempts: 3 } } ]   # first entry is outermost
//	          do: [ { fill: { call: fill_detail } } ]
//	      - log: { call: search_log, background: true }
//	  default_rank:                    # no Entry: a subgraph, only usable by run
//	    do: [ { sort: { call: sort_by_score } } ]
//
// Every task is `name: {...}` with exactly one of call, do, fork, switch,
// run, plus any of if, timeout, optional and background; do tasks and graphs
// also accept wrap. call tasks also accept with (operator config), in and
// out (rename the operator's keys). Task names cannot contain '/'.
//
// # Semantics
//
//   - do runs tasks in order.
//   - fork runs branches concurrently on isolated overlays: a branch does not
//     see its siblings' writes, and two branches may not write the same key
//     (a compile error). When all succeed their writes are committed. The
//     first failure cancels the others and fails the fork at once.
//   - switch evaluates CEL conditions in order and runs the first match. The
//     case named default is the fallback: it has no when and must be last;
//     every other case needs a when. Conditions may only reference keys
//     guaranteed to be set at that point. With no match and no default the
//     switch fails; write `default: { do: [] }` to skip. An evaluation
//     error is the switch task's own failure. A key whose value is a nil
//     pointer is an evaluation error; a nil pointer in a nested field reads
//     as the zero value (also for if).
//   - if skips the task when its CEL condition, evaluated as the task
//     starts, is false. Its writes become "may be absent".
//     An evaluation error is the task's own failure.
//   - run inlines another graph, sharing the board: a graph of the same
//     document by name, or alias.graph for a graph of an imported module
//     (see Documents across modules). A graph without an Entry is a
//     subgraph: it cannot be run directly, must be used by some run, and is
//     checked against the dataflow at each use.
//   - timeout bounds a task. On expiry the task fails with ErrTimeout (also
//     the context.Cause its operators see) and its unfinished work is
//     abandoned: its writes are discarded.
//   - optional tolerates a failure of the task itself: the run continues,
//     the task's writes are discarded and its keys become "may be absent".
//     Cancellation of the caller's ctx is never swallowed.
//   - background starts the task on a snapshot of the board and lets the
//     main path continue at once; Run returns only after it has finished.
//     It never writes back and cannot be optional; failures are logged
//     with slog.ErrorContext. It is cancelled with the run's ctx, not with
//     the task that started it.
//   - wrap applies middleware around tasks (first entry outermost). A
//     middleware is func(ctx, Wrap[In]) error: Wrap carries the wrapped path,
//     In (a struct of `graph:"key"` fields read before the wrapped tasks
//     start, checked like an operator's In) and Next. Each call of next runs
//     the wrapped tasks on a fresh overlay committed only on success, so
//     retries leave nothing behind. The wrapped tasks succeed only if the
//     last attempt did and no middleware returned an error: middleware can observe, retry or reject, but not
//     turn a failure into success (use optional for that). WrapCalls puts
//     middleware around every call task of the document, for observability
//     without editing it; Wrap.Op names the operator.
//
// The tags of In and Out are the operator's port names, which bind keys of
// the same name unless a task's in: / out: renames them (`port: key`). Keep
// graph-level names out of the tags and in the document.
//
// Sequential writes are not protected: a later write to a key replaces an
// earlier one.
// Operators run concurrently across runs and must honour ctx; one that
// ignores ctx is abandoned on timeout but its goroutine runs to completion.
// Panics in operators and middleware become a *TaskError whose Panic and
// Stack describe them;
// panics while decoding `with:` or in Validate become compile problems.
//
// # Compile-time checks
//
// Compile reports every problem, with YAML positions: unknown operators,
// middleware and graphs; bad configs (decoded strictly, then Validate);
// type conflicts on a key; reads of keys nothing writes before that point;
// non-Maybe reads of keys that may be absent; middleware reading keys not
// guaranteed where the wrapped tasks start; two out ports writing one key;
// Entry outputs not written on every path (or, for Maybe outputs, on any
// path); entries without a graph or on a private graph, and root graphs
// without an Entry or a run; CEL syntax and type errors, non-boolean
// conditions, and Go struct types with clashing CEL names; recursive graph
// references; missing, unused or cyclic imports and modules imported twice
// by one document; private graphs run from another document; empty fork and
// switch; optional background tasks; fork branches writing the same key. YAML anchors and aliases are expanded first. A document may hold
// at most 10000 tasks after inlining.
//
// # Documents across modules
//
// A document lists the modules it uses under import:, one alias per module
// id, and runs their graphs as alias.graph; Import options supply the
// documents. Compile still takes one document, the root, and reads no files;
// module ids are opaque strings matched against Import. Names are scoped per
// document, graph names cannot contain '.', and graphs whose names start
// with _ are private to their document. Entries resolve in the root
// document, so an Entry may name alias.graph. Imports are transitive and
// may not form a cycle; an import not provided, an Import nobody imports
// and an unused alias are compile problems, reported with the module id in
// Problem.Module. Imported graphs are inlined like local subgraphs.
//
// Hot reload is left to the caller: Compile the new document and, on
// success, swap it into an atomic.Pointer[Program]. Runs in flight keep the
// Program they started with.
//
// Loops are not supported yet. The compiler tracks dataflow with sets that
// a structured loop construct can extend (a fixpoint over the body), so a
// future `loop: { while, max, do }` task fits without changing the API.
package graph
