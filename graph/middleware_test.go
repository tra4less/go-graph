package graph_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

func isNextNotCalled(err error) bool {
	return err != nil && strings.Contains(err.Error(), "middleware returned without calling next")
}

type tagCfg struct {
	Tag string `yaml:"tag"`
}

// tagView reads key a and takes a config.
type tagView struct {
	Cfg tagCfg `graph:",with"`
	A   string `graph:"a"`
}

func TestMiddleware(t *testing.T) {
	var mu sync.Mutex
	var log []string
	rec := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	main := graph.NewEntry[AIn, ABOut]("main")
	broken := graph.NewEntry[NoIn, ABOut]("broken")
	cat := behaviour(t, main, broken,
		graph.Middleware("tag", func(ctx context.Context, w graph.Wrap[tagView]) error {
			v := w.In
			c := v.Cfg
			rec(fmt.Sprintf("%s>%s(a=%s)", c.Tag, w.Path, v.A))
			err := w.Next(ctx)
			rec("<" + c.Tag)
			return err
		}),
		graph.Middleware("swallow", func(context.Context, graph.Wrap[struct{}]) error { return nil }),
	)
	p := mustCompile(t, cat, `
graphs:
  main:
    wrap: [ { tag: { tag: g } } ]
    do:
      - region:
          wrap: [ { tag: { tag: outer } }, { tag: { tag: inner } } ]
          do: [ { w: { call: put, with: { key: b, val: v } } } ]
  broken:
    do:
      - region:
          wrap: [ swallow ]
          do: [ { w: { call: put, with: { key: b, val: v } } } ]
`)
	if _, err := graph.Run(context.Background(), p, main, AIn{A: "x"}); err != nil {
		t.Fatal(err)
	}
	want := "g>main(a=x) outer>main/region(a=x) inner>main/region(a=x) <inner <outer <g"
	if got := strings.Join(log, " "); got != want {
		t.Errorf("order:\n got %s\nwant %s", got, want)
	}
	_, err := graph.Run(context.Background(), p, broken, NoIn{})
	if !isNextNotCalled(err) {
		t.Errorf("broken: %v", err)
	}
}

func TestMiddlewareCannotSwallowFailure(t *testing.T) {
	main := graph.NewEntry[AIn, ABOut]("main")
	cat := behaviour(t, main, graph.Middleware("swallow", func(ctx context.Context, w graph.Wrap[struct{}]) error {
		_ = w.Next(ctx)
		return nil
	}))
	p := mustCompile(t, cat, `
graphs:
  main:
    do:
      - region:
          wrap: [swallow]
          do: [ { w: { call: put, with: { key: b, val: v, fail: true } } } ]
`)
	_, err := graph.Run(context.Background(), p, main, AIn{A: "x"})
	var ne *graph.TaskError
	if !errors.As(err, &ne) || ne.Path != "main/region/w" {
		t.Fatalf("want the inner failure, got %v", err)
	}
}

func TestRetryDiscardsFailedAttempt(t *testing.T) {
	var mu sync.Mutex
	attempts := map[string]int{}
	bump := func(k string) int {
		mu.Lock()
		defer mu.Unlock()
		attempts[k]++
		return attempts[k]
	}
	type bOut struct {
		B graph.Maybe[string] `graph:"b"`
	}
	main := graph.NewEntry[NoIn, bOut]("main")
	cat := behaviour(t, main,
		// Writes b only on its first attempt.
		graph.Op("first_writes_b", func(context.Context, struct{}) (bOut, error) {
			if bump("w") == 1 {
				return bOut{B: graph.Some("stale")}, nil
			}
			return bOut{}, nil
		}),
		graph.Op("fails_once", func(context.Context, struct{}) (struct{}, error) {
			if bump("f") == 1 {
				return struct{}{}, errors.New("transient")
			}
			return struct{}{}, nil
		}),
		graph.Middleware("retry", func(ctx context.Context, w graph.Wrap[struct{}]) error {
			if err := w.Next(ctx); err == nil {
				return nil
			}
			return w.Next(ctx)
		}),
	)
	p := mustCompile(t, cat, `
graphs:
  main:
    do:
      - region:
          wrap: [retry]
          do:
            - w: { call: first_writes_b }
            - f: { call: fails_once }
`)
	out, err := graph.Run(context.Background(), p, main, NoIn{})
	if err != nil {
		t.Fatal(err)
	}
	if out.B.OK {
		t.Errorf("write from the failed attempt survived: %q", out.B.V)
	}
}

func TestLateNextIsDiscarded(t *testing.T) {
	type bOut struct {
		B graph.Maybe[string] `graph:"b"`
	}
	for _, sync := range []bool{false, true} {
		name := map[bool]string{false: "starts after return", true: "finishes after return"}[sync]
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			lateStarted, finished := make(chan struct{}), make(chan struct{})
			main := graph.NewEntry[NoIn, bOut]("main")
			cat := behaviour(t, main,
				graph.Op("count", func(context.Context, struct{}) (bOut, error) {
					n := calls.Add(1)
					if sync && n == 2 { // the late attempt
						close(lateStarted)
						time.Sleep(20 * time.Millisecond)
					}
					return bOut{B: graph.Some(fmt.Sprint(n))}, nil
				}),
				graph.Middleware("async", func(ctx context.Context, w graph.Wrap[struct{}]) error {
					if !sync {
						returned := make(chan struct{})
						defer close(returned)
						go func() {
							defer close(finished)
							<-returned
							_ = w.Next(ctx)
						}()
						return nil
					}
					err := w.Next(ctx) // a proper synchronous attempt
					go func() {
						defer close(finished)
						_ = w.Next(ctx)
					}()
					<-lateStarted // return while the late attempt is running
					return err
				}),
			)
			p := mustCompile(t, cat, `
graphs:
  main:
    do:
      - region:
          wrap: [async]
          do: [ { w: { call: count } } ]
      - wait: { call: put, with: { sleep: 60ms } }   # keeps the run alive past the late attempt
`)
			out, err := graph.Run(context.Background(), p, main, NoIn{})
			<-finished
			if !sync {
				if !isNextNotCalled(err) || calls.Load() != 0 {
					t.Errorf("err %v, %d region run(s); want next-not-called and none", err, calls.Load())
				}
				return
			}
			if err != nil || out.B.V != "1" {
				t.Errorf("err %v, b %+v; want the synchronous attempt only", err, out.B)
			}
		})
	}
}

// A middleware that returns nil without calling next is blamed for it, even
// when it is not the outermost.
func TestNextNotCalledBlamesTheMiddleware(t *testing.T) {
	main := graph.NewEntry[NoIn, struct{}]("main")
	p := mustCompile(t, behaviour(t, main,
		graph.Middleware("outer", func(ctx context.Context, w graph.Wrap[struct{}]) error { return w.Next(ctx) }),
		graph.Middleware("inner", func(context.Context, graph.Wrap[struct{}]) error { return nil }),
	), `
graphs:
  main:
    do:
      - r: { wrap: [ outer, inner ], do: [ { x: { call: put } } ] }
`)
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	var te *graph.TaskError
	if !isNextNotCalled(err) || !errors.As(err, &te) || te.Middleware != "inner" || te.Op != "" || te.Path != "main/r" {
		t.Errorf("want next-not-called from inner at main/r, got %v (%+v)", err, te)
	}
}

// A middleware that rejects after a successful attempt fails the region,
// and the attempt's writes must not reach the run's result.
func TestRejectAfterSuccessLeavesNothing(t *testing.T) {
	type bOut struct {
		B graph.Maybe[string] `graph:"b"`
	}
	main := graph.NewEntry[NoIn, bOut]("main")
	cat := behaviour(t, main,
		graph.Op("writes_b", func(context.Context, struct{}) (bOut, error) {
			return bOut{B: graph.Some("leaked")}, nil
		}),
		graph.Middleware("reject", func(ctx context.Context, w graph.Wrap[struct{}]) error {
			if err := w.Next(ctx); err != nil {
				return err
			}
			return errors.New("rejected")
		}),
	)
	p := mustCompile(t, cat, `
graphs:
  main:
    do:
      - region:
          wrap: [reject]
          do:
            - w: { call: writes_b }
`)
	out, err := graph.Run(context.Background(), p, main, NoIn{})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want the rejection, got %v", err)
	}
	if strings.Count(err.Error(), "graph:") != 1 {
		t.Errorf("error prefix repeated: %v", err)
	}
	if out.B.OK {
		t.Errorf("write of the rejected region survived: %q", out.B.V)
	}
}

type spanKey struct{}

// tracer records which middleware saw which call, in order.
type tracer struct {
	mu   sync.Mutex
	seen []string
}

func (tr *tracer) note(s string) {
	tr.mu.Lock()
	tr.seen = append(tr.seen, s)
	tr.mu.Unlock()
}

func (tr *tracer) middleware(name string) graph.Option {
	return graph.Middleware(name, func(ctx context.Context, w graph.Wrap[struct{}]) error {
		tr.note(name + " " + w.Path + " " + w.Op)
		return w.Next(context.WithValue(ctx, spanKey{}, name))
	})
}

func TestWrapCalls(t *testing.T) {
	tr := &tracer{}
	var opCtx []string
	ctxOp := graph.Op("ctx_op", func(ctx context.Context, _ struct{}) (struct{}, error) {
		v, _ := ctx.Value(spanKey{}).(string)
		_, deadline := ctx.Deadline()
		opCtx = append(opCtx, v)
		if !deadline {
			return struct{}{}, errors.New("want the task's timeout inside the middleware")
		}
		return struct{}{}, nil
	})
	main := graph.NewEntry[NoIn, struct{}]("main")
	p := mustCompile(t, optsOf(main, ctxOp, tr.middleware("outer"), tr.middleware("inner"),
		graph.WrapCalls("outer", "inner")), `
graphs:
  main:
    do:
      - a: { call: ctx_op, timeout: 1s }
      - region:
          wrap: [outer]
          do: [ { b: { call: ctx_op, timeout: 1s } } ]
`)
	if _, err := graph.Run(context.Background(), p, main, NoIn{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"outer main/a ctx_op", "inner main/a ctx_op",
		"outer main/region ", // a region wrap: no Op
		"outer main/region/b ctx_op", "inner main/region/b ctx_op",
	}
	if strings.Join(tr.seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("seen:\n%s\nwant:\n%s", strings.Join(tr.seen, "\n"), strings.Join(want, "\n"))
	}
	// The innermost middleware's ctx reaches the operator.
	if strings.Join(opCtx, ",") != "inner,inner" {
		t.Errorf("operator ctx values %v", opCtx)
	}
}

// WrapCalls middleware follow wrap's rules: retried attempts leave nothing
// behind, and the outcome is the last attempt's.
func TestWrapCallsRetry(t *testing.T) {
	var calls int
	flaky := graph.Op("flaky", func(context.Context, struct{}) (struct {
		N int `graph:"n"`
	}, error) {
		calls++
		if calls == 1 {
			return struct {
				N int `graph:"n"`
			}{N: -1}, errors.New("transient")
		}
		return struct {
			N int `graph:"n"`
		}{N: calls}, nil
	})
	retry := graph.Middleware("retry", func(ctx context.Context, w graph.Wrap[struct{}]) error {
		if err := w.Next(ctx); err == nil {
			return nil
		}
		return w.Next(ctx)
	})
	type out struct {
		N int `graph:"n"`
	}
	main := graph.NewEntry[NoIn, out]("main")
	p := mustCompile(t, optsOf(main, flaky, retry, graph.WrapCalls("retry")),
		"graphs:\n  main:\n    do: [ { f: { call: flaky } } ]\n")
	res, err := graph.Run(context.Background(), p, main, NoIn{})
	if err != nil || res.N != 2 {
		t.Fatalf("n = %d, err %v", res.N, err)
	}
}

func TestWrapCallsChecks(t *testing.T) {
	needsUser := graph.Middleware("needs_user", func(ctx context.Context, w graph.Wrap[struct {
		User *User `graph:"user"`
	}]) error {
		return w.Next(ctx)
	})
	nop := graph.Middleware("nop", func(ctx context.Context, w graph.Wrap[struct{}]) error { return w.Next(ctx) })

	// The middleware's In is checked at every call task.
	_, err := graph.Compile([]byte(answerDoc), append(baseOptions(), needsUser, graph.WrapCalls("needs_user"))...)
	var ce *graph.CompileError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), `main/user: middleware "needs_user" reads "user", but nothing before this point writes it`) {
		t.Errorf("view check: %v", err)
	}

	bad := []struct {
		name string
		opts []graph.Option
		want string
	}{
		{"unknown", optsOf(graph.WrapCalls("nope")), `WrapCalls: no middleware "nope"`},
		{"listed twice", optsOf(nop, graph.WrapCalls("nop", "nop")), `middleware "nop" listed twice`},
		{"given twice", optsOf(nop, graph.WrapCalls("nop"), graph.WrapCalls("nop")), "WrapCalls given twice"},
		{"in Override", optsOf(nop, graph.Override(graph.WrapCalls("nop"))), "Override cannot contain WrapCalls"},
	}
	for _, c := range bad {
		_, err := graph.Compile([]byte(answerDoc), append(baseOptions(), c.opts...)...)
		if err == nil || errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want option error %q, got %v", c.name, c.want, err)
		}
	}

	// Override replaces a middleware used by WrapCalls.
	tr := &tracer{}
	p := mustCompile(t, baseOptions(), answerDoc, nop, graph.WrapCalls("nop"),
		graph.Override(graph.Middleware("nop", func(ctx context.Context, w graph.Wrap[struct{}]) error {
			tr.note(w.Op)
			return w.Next(ctx)
		})))
	if _, err := graph.Run(context.Background(), p, Main, MainIn{UID: "bob", Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tr.seen, ","); got != "load_user,parse,plain_answer" {
		t.Errorf("override saw %q", got)
	}
}

// A middleware's own failure names the middleware; under WrapCalls it also
// names the operator it wraps.
func TestMiddlewareFailureNamesMiddleware(t *testing.T) {
	main := graph.NewEntry[NoIn, struct{}]("main")
	cat := behaviour(t, main,
		graph.Op("ok", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil }),
		graph.Middleware("reject", func(ctx context.Context, w graph.Wrap[struct{}]) error {
			if err := w.Next(ctx); err != nil {
				return err
			}
			return errors.New("rejected")
		}),
		graph.WrapCalls("reject"),
	)
	p := mustCompile(t, cat, "graphs:\n  main:\n    do:\n      - x: { call: ok }\n")
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	var te *graph.TaskError
	if !errors.As(err, &te) || te.Op != "ok" || te.Middleware != "reject" || te.Path != "main/x" {
		t.Fatalf("err %v (%+v)", err, te)
	}
	if want := "graph: main/x (ok, middleware reject): rejected"; err.Error() != want {
		t.Errorf("message %q, want %q", err.Error(), want)
	}
}
