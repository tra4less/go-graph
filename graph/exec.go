package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// plan is a compiled, immutable piece of a graph. exec runs it on board b.
// Every error it returns is a *TaskError.
type plan interface {
	exec(ctx context.Context, r *runState, b *board) error
}

// taskNode is one named task. It owns the per-task attributes (if,
// timeout, optional, background) and the switch decision. A switch
// task has cases and a nil body; every other task has a body (a callNode,
// seqNode, forkNode or wrapNode) and nil cases.
type taskNode struct {
	path       string
	op         string
	timeout    time.Duration
	optional   bool
	background bool
	cond       *condition // `if:`; nil when absent
	body       plan
	cases      *switchCases
}

func (t *taskNode) exec(ctx context.Context, r *runState, b *board) error {
	if err := ctx.Err(); err != nil {
		return t.fail(err)
	}
	if t.background {
		t.startBackground(ctx, r, b)
		return nil
	}
	run, err := t.shouldRun(b)
	if err == nil && !run {
		return nil // if: false, skipped
	}
	target := b
	if err == nil {
		if t.optional || t.timeout > 0 {
			target = b.child()
		}
		err = t.execBody(ctx, r, target)
	}
	switch {
	case err == nil:
		if target != b {
			target.commit()
		}
		return nil
	case t.optional && ctx.Err() == nil:
		// optional: the task's own failure is ignored and its writes dropped.
		// The caller's cancellation must still stop the run.
		return nil
	}
	return err
}

// shouldRun evaluates the task's `if:` on b. A task without one always runs. An
// evaluation error (a nil pointer in the expression) is the task's own
// failure, so optional applies to it.
func (t *taskNode) shouldRun(b *board) (bool, error) {
	if t.cond == nil {
		return true, nil
	}
	ok, err := t.cond.eval(b)
	if err != nil {
		return false, newTaskError(t.path, t.op, fmt.Errorf("if %q: %w", t.cond.src, err))
	}
	return ok, nil
}

// execBody picks the switch case, if any, and executes the body under the
// task's timeout.
func (t *taskNode) execBody(ctx context.Context, r *runState, b *board) error {
	body := t.body
	if t.cases != nil {
		c, err := t.cases.pick(b)
		if err != nil {
			return err
		}
		if c == nil {
			return t.fail(errNoCase)
		}
		body = c.body
	}
	if t.timeout <= 0 {
		return body.exec(ctx, r, b)
	}
	return t.runWithTimeout(ctx, r, b, body)
}

// runWithTimeout abandons body when the timeout fires. Abandoned work keeps
// running until it notices ctx, but it writes only to b, an overlay the
// caller discards on error.
func (t *taskNode) runWithTimeout(ctx context.Context, r *runState, b *board, body plan) error {
	tctx, cancel := context.WithTimeoutCause(ctx, t.timeout, ErrTimeout)
	defer cancel()
	done := make(chan error, 1) // buffered: an abandoned body must not block
	r.prog.spawn(func() error { return body.exec(tctx, r, b) }, func(err error) {
		if err != nil {
			err = t.fail(err)
		}
		done <- err
	})
	select {
	case err := <-done:
		if err != nil && ctx.Err() == nil && context.Cause(tctx) == ErrTimeout {
			return t.fail(ErrTimeout)
		}
		return err
	case <-tctx.Done():
		if err := ctx.Err(); err != nil {
			return t.fail(err)
		}
		return t.fail(ErrTimeout)
	}
}

// startBackground starts a background task on a snapshot of the board and
// returns at once. The run waits for it before Run returns. It never writes
// back, and logs failure with slog (ErrorContext, so request ids in ctx
// reach the log). Its `if:` is part of the background work: evaluated on
// the snapshot, a false condition just ends it, an evaluation error is
// reported like any failure.
//
// Its context keeps ctx's values (spans set by enclosing middleware, trace
// ids) but not ctx's cancellation, which ends when the enclosing region
// does: it is cancelled only with the run's own context, whose cause it
// carries (context.Cause).
func (t *taskNode) startBackground(ctx context.Context, r *runState, b *board) {
	p := r.prog
	snap := b.snapshot()
	bg, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	stop := context.AfterFunc(r.ctx, func() { cancel(context.Cause(r.ctx)) })
	r.background.add()
	p.spawn(func() error {
		if run, err := t.shouldRun(snap); !run {
			return err
		}
		return t.execBody(bg, r, snap)
	}, func(err error) {
		if err != nil {
			logBackgroundError(bg, t.fail(err))
		}
		stop()
		cancel(nil)
		r.background.done()
	})
}

// fail attributes err to t unless it already belongs to a task inside t.
func (t *taskNode) fail(err error) *TaskError {
	if ne, ok := err.(*TaskError); ok {
		return ne
	}
	return newTaskError(t.path, t.op, err)
}

type seqNode struct{ tasks []*taskNode }

func (n *seqNode) exec(ctx context.Context, r *runState, b *board) error {
	for _, t := range n.tasks {
		if err := t.exec(ctx, r, b); err != nil {
			return err
		}
	}
	return nil
}

type callNode struct {
	path    string
	def     *opDef
	cfg     any
	in, out map[string]string // key renames from the task's in/out
}

func (n *callNode) exec(ctx context.Context, _ *runState, b *board) error {
	err := guard(func() error {
		out, err := n.def.invoke(ctx, n.def.in.read(b, n.in, n.cfg))
		if err != nil {
			return err
		}
		n.def.out.write(b, n.out, out)
		return nil
	})
	if err != nil {
		return newTaskError(n.path, n.def.name, err)
	}
	return nil
}

// forkNode runs its branches concurrently, each on its own overlay; the
// compiler guarantees no two branches write the same key. When all succeed
// the overlays are committed. The first failure cancels the other branches
// and fails the fork without waiting for them.
type forkNode struct{ branches []*taskNode }

func (n *forkNode) exec(ctx context.Context, r *runState, b *board) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		i   int
		err error
	}
	results := make(chan result, len(n.branches)) // buffered: losers must not block
	boards := make([]*board, len(n.branches))
	for i, br := range n.branches {
		boards[i] = b.child()
		r.prog.spawn(func() error { return br.exec(ctx, r, boards[i]) }, func(err error) {
			if err != nil {
				err = br.fail(err)
			}
			results <- result{i, err}
		})
	}
	for range n.branches {
		if res := <-results; res.err != nil {
			return res.err
		}
	}
	for _, bd := range boards {
		bd.commit()
	}
	return nil
}

type switchCase struct {
	name string
	cond *condition // nil for the default case
	body *seqNode
}

// switchCases are the cases of a switch task, in order.
type switchCases struct {
	path  string
	cases []switchCase
}

// pick returns the first case whose condition holds, or nil.
func (n *switchCases) pick(b *board) (*switchCase, error) {
	for i := range n.cases {
		c := &n.cases[i]
		if c.cond == nil {
			return c, nil
		}
		ok, err := c.cond.eval(b)
		if err != nil {
			return nil, newTaskError(n.path+"/"+c.name, "", fmt.Errorf("when %q: %w", c.cond.src, err))
		}
		if ok {
			return c, nil
		}
	}
	return nil, nil
}

type boundMw struct {
	def *mwDef
	cfg any
}

// wrapNode runs its body inside an onion of middleware; mws[0] is outermost.
//
// Each call of next runs the body on a fresh overlay that is committed only
// on success, so a retried attempt leaves nothing behind. Calls are
// serialized. The region succeeds only if the last attempt did and no
// middleware returned an error: middleware can observe, retry or reject,
// but not turn a failure into success. Successful attempts commit into an
// overlay of the whole region, published only when the region succeeds, so
// a middleware that rejects after a successful attempt leaves nothing
// behind either.
//
// next must be called before the middleware returns. An attempt still
// running when the outermost middleware returns does not count: its writes
// are discarded and the region's outcome is the last attempt that finished
// before, so the result never depends on goroutine scheduling.
type wrapNode struct {
	path string
	op   string // the operator, when the region is one call task (WrapCalls)
	mws  []boundMw
	body plan
}

func (n *wrapNode) exec(ctx context.Context, r *runState, b *board) error {
	var (
		runMu    sync.Mutex // serializes attempts
		stateMu  sync.Mutex // guards the two fields below; never held while user code runs
		last     = errNextNotCalled
		returned bool // the outermost middleware has returned
	)
	region := b.child() // published only if the region as a whole succeeds
	isReturned := func() bool {
		stateMu.Lock()
		defer stateMu.Unlock()
		return returned
	}
	next := nextFunc(func(ctx context.Context) error {
		runMu.Lock()
		defer runMu.Unlock()
		if isReturned() {
			return errLateNext
		}
		attempt := region.child()
		err := n.body.exec(ctx, r, attempt)
		stateMu.Lock()
		defer stateMu.Unlock()
		if returned {
			return errLateNext // finished too late to count; attempt is discarded
		}
		if err == nil {
			attempt.commit()
		}
		last = err
		return err
	})
	for i := len(n.mws) - 1; i >= 0; i-- {
		next = n.mws[i].wrap(n.path, n.op, b, next)
	}
	err := next(ctx)

	stateMu.Lock()
	returned = true
	outcome := last
	stateMu.Unlock()
	switch {
	case err != nil:
		return err
	case outcome == errNextNotCalled:
		// Every middleware called next, but the body had not finished when
		// the outermost one returned.
		return newTaskError(n.path, "", errNextNotCalled)
	case outcome != nil:
		return outcome
	}
	region.commit()
	return nil
}

var errLateNext = errors.New("next called after the middleware returned")

// wrap returns a Next that runs m around inner, attributing m's own errors
// and panics to m, and so a return of nil without calling inner: the
// middleware outside m must not take that for a successful region. m's In
// is read from b when m starts.
func (m boundMw) wrap(path, op string, b *board, inner nextFunc) nextFunc {
	return func(ctx context.Context) error {
		var called atomic.Bool // inner may be called on another goroutine
		next := nextFunc(func(ctx context.Context) error {
			called.Store(true)
			return inner(ctx)
		})
		err := guard(func() error {
			return m.def.invoke(ctx, path, op, m.def.in.read(b, nil, m.cfg), next)
		})
		if err == nil && !called.Load() {
			err = errNextNotCalled
		}
		if err == nil {
			return nil
		}
		if ne, ok := err.(*TaskError); ok {
			return ne // passed through from inner
		}
		te := newTaskError(path, op, err)
		te.Middleware = m.def.name
		return te
	}
}

// guard runs user code, converting a panic into a *panicError.
func guard(f func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &panicError{value: v, stack: debug.Stack()}
		}
	}()
	return f()
}

// runState is the per-run context threaded through plan execution. It is
// shared by every goroutine of the run.
type runState struct {
	prog       *Program
	ctx        context.Context // the run's context, as given to Run
	background *inflight       // background tasks Run waits for
}

func logBackgroundError(ctx context.Context, e *TaskError) {
	slog.Default().ErrorContext(ctx, "graph: background task failed", "path", e.Path, "op", e.Op, "middleware", e.Middleware, "err", e.Err)
}
