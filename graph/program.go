package graph

import (
	"context"
	"errors"
	"fmt"
)

// Program is a compiled graph document. It is immutable and safe for
// concurrent use. Run runs one of its entries.
type Program struct {
	entries    map[any]*entryPlan // by *Entry
	goroutines inflight           // every goroutine started by runs of this Program
}

// entryPlan is the executable form of an entry graph.
type entryPlan struct {
	in, out shape
	body    plan
}

// Run runs the graph of entry e once on p, with input in.
//
// p must have been compiled with e among its options, otherwise Run returns
// an error. Errors from the graph itself are *TaskError. Once the graph has
// started, out holds whatever the final blackboard has, even on error
// (missing fields are zero), so partial results can be inspected.
//
// Run returns when the graph and every background task it started have
// finished. Work abandoned by a timeout may still be running; see
// Program.Drain.
func Run[In, Out any](ctx context.Context, p *Program, e *Entry[In, Out], in In) (out Out, err error) {
	var g *entryPlan
	if e != nil && p != nil {
		g = p.entries[e]
	}
	if g == nil {
		name := ""
		if e != nil {
			name = e.graph
		}
		return out, fmt.Errorf("%w: %q", errUnknownEntry, name)
	}
	b := newBoard()
	g.in.write(b, nil, in)
	r := &runState{prog: p, ctx: ctx, background: new(inflight)}
	err = g.body.exec(ctx, r, b)
	// Background tasks started by the run end with it. Ones started later by
	// abandoned work are not waited for here; Program.Drain covers them.
	_ = r.background.wait(context.Background())
	return g.out.read(b, nil, nil).(Out), err
}

// Drain waits until every goroutine started by runs of this Program has
// exited, or ctx is done. Run already waits for its background tasks; what
// can outlive a run is work it abandoned — a body still running when its
// timeout fired, fork branches still running when a sibling failed — when
// that work ignores ctx. Call Drain at shutdown, and on a Program replaced
// by hot reload if such work must finish.
//
// With a ctx that is already done, Drain does not block: it returns nil if
// the Program is idle and ctx's error otherwise.
func (p *Program) Drain(ctx context.Context) error { return p.goroutines.wait(ctx) }

// spawn runs f on a goroutine counted for Drain and passes its result to
// report. If f calls runtime.Goexit (t.FailNow in a test, say), which no
// recover can stop, report still runs, with errGoexit, so whoever waits for
// the result is not stranded.
func (p *Program) spawn(f func() error, report func(error)) {
	p.goroutines.add()
	go func() {
		defer p.goroutines.done()
		err := errGoexit
		defer func() { report(err) }()
		err = f()
	}()
}

var errGoexit = errors.New("runtime.Goexit called")
