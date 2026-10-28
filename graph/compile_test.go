package graph_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

type topK struct {
	K int `yaml:"k"`
}

func (c topK) Validate() error {
	if c.K <= 0 {
		return errors.New("k must be > 0")
	}
	return nil
}

// panicCfg's Validate panics when m is missing (a nil map write).
type panicCfg struct {
	M map[string]int `yaml:"m"`
}

func (c panicCfg) Validate() error {
	c.M["seen"]++
	return nil
}

// embedsMaybe inherits Maybe's methods but is a plain struct, not a Maybe.
type embedsMaybe struct{ graph.Maybe[int] }

type ptrCfg struct {
	Limit int `yaml:"limit"`
}

func (c *ptrCfg) Validate() error {
	if c.Limit == 0 {
		return errors.New("limit required")
	}
	return nil
}

func compileOptions(t *testing.T) []graph.Option {
	t.Helper()
	return optsOf(append(baseOps(),
		graph.NewEntry[MainIn, struct{}]("main"),
		graph.Op("two_out", func(context.Context, struct{}) (struct {
			A string `graph:"a"`
			B string `graph:"b"`
		}, error) {
			return struct {
				A string `graph:"a"`
				B string `graph:"b"`
			}{}, nil
		}),
		graph.Op("topk", func(context.Context, struct {
			Cfg topK `graph:",with"`
		}) (struct{}, error) {
			return struct{}{}, nil
		}),
		graph.Op("user_as_int", func(context.Context, struct{}) (struct {
			User int `graph:"user"`
		}, error) {
			return struct {
				User int `graph:"user"`
			}{}, nil
		}),
		graph.Op("maybe_user", func(context.Context, struct{}) (struct {
			User graph.Maybe[*User] `graph:"user"`
		}, error) {
			return struct {
				User graph.Maybe[*User] `graph:"user"`
			}{}, nil
		}),
		graph.Op("typo_reader", func(context.Context, struct {
			X graph.Maybe[string] `graph:"usr"`
		}) (struct{}, error) {
			return struct{}{}, nil
		}),
		graph.Op("embeds_opt", func(context.Context, struct {
			X embedsMaybe `graph:"x"`
		}) (struct{}, error) {
			return struct{}{}, nil
		}),
		graph.Op("panicky_cfg", func(context.Context, struct {
			Cfg panicCfg `graph:",with"`
		}) (struct{}, error) {
			return struct{}{}, nil
		}),
		graph.Op("ptr_cfg", func(context.Context, struct {
			Cfg *ptrCfg `graph:",with"`
		}) (struct{}, error) {
			return struct{}{}, nil
		}),
		graph.Middleware("noop", func(ctx context.Context, w graph.Wrap[struct{}]) error { return w.Next(ctx) }),
		graph.Middleware("needs_user", func(ctx context.Context, w graph.Wrap[struct {
			User *User `graph:"user"`
		}]) error {
			return w.Next(ctx)
		}),
	)...)
}

func TestCompileProblems(t *testing.T) {
	cat := compileOptions(t)
	cases := []struct {
		name, src, want string
	}{
		{"yaml syntax", "graphs: [", "did not find expected"},
		{"unknown top field", "graph: {}", `unknown field "graph"`},
		{"unknown op", g(`- x: { call: nope }`), `unknown operator "nope"`},
		{"unknown middleware", g(`- x: { wrap: [nope], do: [] }`), `unknown middleware "nope"`},
		{"unknown graph", g(`- x: { run: nope }`), `unknown graph "nope"`},
		{"two kinds", g(`- x: { call: parse, do: [] }`), "more than one of"},
		{"no kind", g(`- x: { timeout: 1s }`), "needs one of"},
		{"unknown task field", g(`- x: { call: parse, retry: 3 }`), `unknown field "retry"`},
		{"bad timeout", g(`- x: { call: parse, timeout: soon }`), "positive duration"},
		{"duplicate task", g(`- x: { call: parse }`, `- x: { call: parse }`), `duplicate task name "x"`},
		{"wrap without do", g(`- x: { call: parse, wrap: [noop] }`), "wrap requires do"},
		{"with on do", g(`- x: { do: [], with: {} }`), "only allowed on call"},
		{"cfg unknown field", g(`- x: { call: topk, with: { k: 1, j: 2 } }`), "field j not found"},
		{"cfg validate", g(`- x: { call: topk, with: { k: 0 } }`), "k must be > 0"},
		{"cfg on plain op", g(`- x: { call: parse, with: { k: 1 } }`), "takes no config"},
		{"rename unknown", g(`- x: { call: parse, in: { nope: query } }`), `no in key "nope"`},
		{"two out ports into one key", g(`- x: { call: two_out, out: { a: x, b: x } }`), `out: ports "a" and "b" of operator "two_out" both write "x"`},
		{"out rename onto another port", g(`- x: { call: two_out, out: { a: b } }`), `ports "a" and "b" of operator "two_out" both write "b"`},
		{"no producer", g(`- x: { call: vip_answer }`), `reads "user", but nothing before this point writes it`},
		{"typo in maybe read", g(`- x: { call: typo_reader }`), `reads "usr", but nothing`},
		{"may be absent needs Maybe", g(`- u: { call: load_user, optional: true }`, `- p: { call: parse }`, `- x: { call: plain_answer }`),
			`reads "user", which may be absent`},
		{"Maybe output may be absent", g(`- u: { call: maybe_user }`, `- p: { call: parse }`, `- x: { call: plain_answer }`),
			`reads "user", which may be absent`},
		{"type conflict", g(`- u: { call: load_user }`, `- i: { call: user_as_int }`), `key "user" is int here but *graph_test.User`},
		{"parsed from optional", g(`- u: { call: load_user }`, `- p: { call: parse, optional: true }`, `- x: { call: vip_answer }`),
			`reads "parsed", which may be absent`},
		{"fork branch writes are visible after the fork", g(`- f: { fork: [ { u: { call: load_user } }, { p: { call: parse } } ] }`,
			`- x: { call: vip_answer }`, `- y: { call: nope }`), `unknown operator "nope"`},
		{"siblings cannot see each other", g(`- f: { fork: [ { u: { call: load_user } }, { x: { call: plain_answer } } ] }`),
			`reads "user", but nothing`},
		{"fork branches write the same key", g(`- f: { fork: [ { a: { call: load_user } }, { b: { do: [ { c: { call: load_user, optional: true } } ] } } ] }`),
			`fork branches "a" and "b" both write "user"`},
		{"fork branches rename into the same key", g(`- u: { call: load_user }`,
			`- f: { fork: [ { a: { call: parse } }, { b: { call: parse, out: { parsed: other } } }, { c: { call: parse, out: { parsed: other } } } ] }`),
			`fork branches "b" and "c" both write "other"`},
		{"key from one case is maybe", g(
			`- s: { switch: [ { c1: { when: "uid == 'a'", do: [ { u: { call: load_user } } ] } }, { default: { do: [] } } ] }`,
			`- p: { call: parse }`, `- x: { call: vip_answer }`), `reads "user", which may be absent`},
		{"cel not bool", g(`- s: { switch: [ { c1: { when: "uid", do: [] } } ] }`), "condition must be bool"},
		{"cel bad field", g(`- u: { call: load_user }`, `- s: { switch: [ { c1: { when: "user.Age > 1", do: [] } } ] }`), "Age"},
		{"cel maybe key", g(`- s: { switch: [ { c1: { when: "hint == 'x'", do: [] } } ] }`), `key "hint" may be absent here`},
		{"default not last", g(`- s: { switch: [ { default: { do: [] } }, { c: { when: "true", do: [] } } ] }`), "the default case must be last"},
		{"case without when", g(`- s: { switch: [ { other: { do: [] } } ] }`), `case "other" needs a when`},
		{"default with when", g(`- s: { switch: [ { default: { when: "true", do: [] } } ] }`), "the default case takes no when"},
		{"old fork form", g(`- f: { fork: { branches: [] } }`), "expected a list of tasks"},
		{"old run form", g(`- r: { run: { graph: main } }`), "expected a non-empty string"},
		{"recursion", "graphs:\n  main:\n    do: [ { x: { run: main } } ]\n", "recursive graph reference: main -> main"},
		{"subgraph checked at the call site", "graphs:\n  main:\n    do: [ { x: { run: sub } } ]\n  sub:\n    do: [ { a: { call: plain_answer } } ]\n",
			`main/x/a: operator "plain_answer" reads "user"`},
		{"middleware view unwritten", g(`- r: { wrap: [needs_user], do: [ { u: { call: load_user } } ] }`),
			`middleware "needs_user" reads "user", but nothing before this point writes it`},
		{"middleware view may be absent", g(`- u: { call: load_user, optional: true }`, `- r: { wrap: [needs_user], do: [] }`),
			`middleware "needs_user" reads "user", which may be absent`},
		{"cfg panics", g(`- x: { call: panicky_cfg }`), "panic: assignment to entry in nil map"},
		{"no input field", "graphs:\n  main:\n    input: [uid]\n    do: []\n", `unknown field "input"`},
		{"entry graph missing", "graphs:\n  other:\n    do: []\n", `Entry "main": unknown graph "main"`},
		{"background writes are not visible", g(`- u: { call: load_user, background: true }`, `- p: { call: parse }`, `- x: { call: vip_answer }`),
			`reads "user", but nothing before this point writes it`},
		{"background not optional", g(`- x: { call: parse, background: true, timeout: 1s, optional: true }`), "cannot be optional"},
		{"empty fork", g(`- x: { fork: [] }`), "at least one branch"},
		{"empty switch", g(`- x: { switch: [] }`), "at least one case"},
		{"embedded Maybe is not Maybe", g(`- x: { call: embeds_opt }`), `reads "x", but nothing before this point writes it`},
		{"pointer cfg validated", g(`- x: { call: ptr_cfg }`), "limit required"},
		{"orphan graph", "graphs:\n  main:\n    do: []\n  sub:\n    do: []\n", `graph "sub" has no Entry and no graph runs it`},
		{"run only by an orphan", "graphs:\n  main:\n    do: []\n  a:\n    do: [ { r: { run: b } } ]\n  b:\n    do: []\n", `graph "b" is run only by graphs no Entry reaches`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := graph.Compile([]byte(c.src), cat...)
			var ce *graph.CompileError
			if !errors.As(err, &ce) {
				t.Fatalf("want CompileError, got %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q in:\n%v", c.want, err)
			}
		})
	}
}

func TestCompileReportsAllProblemsWithPositions(t *testing.T) {
	_, err := graph.Compile([]byte(g(`- a: { call: nope1 }`, `- b: { call: nope2 }`)), compileOptions(t)...)
	var ce *graph.CompileError
	if !errors.As(err, &ce) || len(ce.Problems) != 2 {
		t.Fatalf("got %v", err)
	}
	if p := ce.Problems[1]; p.Path != "main/b" || p.Line != 5 {
		t.Errorf("problem %+v", p)
	}
}

func TestEntryChecks(t *testing.T) {
	type out struct {
		User   *User               `graph:"user"`
		Hint   graph.Maybe[string] `graph:"hint"`
		Answer graph.Maybe[string] `graph:"answer"`
	}
	type badOut struct {
		Answer graph.Maybe[int] `graph:"answer"`
	}
	type missing struct {
		Nope graph.Maybe[string] `graph:"nope"`
	}
	cases := []struct {
		name  string
		entry graph.Option
		src   string
		want  string
	}{
		{"required output may be absent", graph.NewEntry[MainIn, out]("main"),
			g(`- u: { call: load_user, optional: true }`), `Entry output "user" may be absent`},
		{"output never written", graph.NewEntry[MainIn, missing]("main"),
			g(`- u: { call: load_user }`), `Entry output "nope" is never written`},
		{"output type conflict", graph.NewEntry[MainIn, badOut]("main"),
			g(`- u: { call: load_user }`, `- p: { call: parse }`, `- a: { call: plain_answer }`), `key "answer" is int here`},
		{"input type conflict", graph.NewEntry[struct {
			User string `graph:"user"`
		}, struct{}]("main"), g(`- u: { call: load_user }`), `key "user" is *graph_test.User here but string`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := graph.Compile([]byte(c.src), append(baseOps(), c.entry)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q in:\n%v", c.want, err)
			}
		})
	}

	// Maybe outputs only need to be written on some path; the input Maybe hint
	// passes straight through.
	ok := graph.NewEntry[MainIn, out]("main")
	p := mustCompile(t, optsOf(append(baseOps(), ok)...),
		g(`- u: { call: load_user }`, `- p: { call: parse }`,
			`- s: { switch: [ { vip: { when: "user.Level > 3", do: [ { a: { call: vip_answer } } ] } }, { default: { do: [] } } ] }`))
	res, err := graph.Run(context.Background(), p, ok, MainIn{UID: "bob", Query: "q", Hint: graph.Some("h")})
	if err != nil {
		t.Fatal(err)
	}
	if res.User.Name != "bob" || res.Hint.V != "h" || res.Answer.OK {
		t.Errorf("out %+v", res)
	}
}

func TestInliningBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("graphs:\n")
	const depth = 20 // 2^20 tasks if fully inlined
	for i := range depth {
		fmt.Fprintf(&b, "  g%d:\n    do:\n", i)
		if i == depth-1 {
			b.WriteString("      - leaf: { call: put }\n")
			continue
		}
		fmt.Fprintf(&b, "      - l: { run: g%d }\n      - r: { run: g%d }\n", i+1, i+1)
	}
	start := time.Now()
	_, err := graph.Compile([]byte(b.String()), behaviour(t, graph.NewEntry[NoIn, struct{}]("g0"))...)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want budget error, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Compile took %v", d)
	}
}

func TestKeyTypesArePerEntry(t *testing.T) {
	type sIn struct {
		X string `graph:"x"`
	}
	type iIn struct {
		X int `graph:"x"`
	}
	a := graph.NewEntry[sIn, struct{}]("a")
	b := graph.NewEntry[iIn, struct{}]("b")
	if _, err := graph.Compile([]byte("graphs:\n  a:\n    do: []\n  b:\n    do: []\n"), a, b); err != nil {
		t.Errorf("unrelated entries must not share key types: %v", err)
	}
}

type countedCfg struct {
	Items []int `yaml:"items"`
}

var validated atomic.Int32

func (countedCfg) Validate() error {
	validated.Add(1)
	return nil
}

// Work that does not depend on where a subgraph is inlined (decoding its
// configs, compiling its conditions) is done once, so a small document that
// inlines a heavy subgraph thousands of times compiles quickly.
func TestInlinedWorkIsShared(t *testing.T) {
	const depth, items, cases = 11, 5000, 200 // 2^11 inlined copies of the leaf
	var b strings.Builder
	b.WriteString("graphs:\n")
	for i := range depth {
		fmt.Fprintf(&b, "  s%d:\n    do:\n      - l: { run: s%d }\n      - r: { run: s%d }\n", i, i+1, i+1)
	}
	fmt.Fprintf(&b, "  s%d:\n    do:\n      - c:\n          call: counted\n          with: { items: [", depth)
	for i := range items {
		fmt.Fprintf(&b, "%d,", i)
	}
	b.WriteString("] }\n      - w:\n          switch:\n")
	for i := range cases {
		fmt.Fprintf(&b, "            - c%d: { when: \"n > %d\", do: [] }\n", i, i)
	}
	type in struct {
		N int `graph:"n"`
	}
	validated.Store(0)
	start := time.Now()
	_, err := graph.Compile([]byte(b.String()), graph.NewEntry[in, struct{}]("s0"),
		graph.Op("counted", func(context.Context, struct {
			Cfg countedCfg `graph:",with"`
		}) (struct{}, error) {
			return struct{}{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if n := validated.Load(); n != 1 {
		t.Errorf("config validated %d times, want once", n)
	}
	// About 0.4s (2s with -race); over 20s when conditions are recompiled
	// at every inlined copy.
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Compile took %v", d)
	}
}

// Only writes inside the branches count: a key written before the fork may be
// rewritten by one branch, and a background branch never writes.
func TestForkWritersOnlyCountBranches(t *testing.T) {
	mustCompile(t, baseOptions(), g(
		`- u: { call: load_user }`,
		`- f: { fork: [ { a: { call: load_user } }, { b: { call: parse } }, { c: { call: load_user, background: true, timeout: 1s } } ] }`,
		`- x: { call: vip_answer }`))
}

func TestConditionNamesNonIdentifierKey(t *testing.T) {
	type out struct {
		ID int `graph:"user-id"`
	}
	opts := optsOf(graph.NewEntry[struct{}, struct{}]("main"),
		graph.Op("put_id", func(context.Context, struct{}) (out, error) { return out{}, nil }))
	_, err := graph.Compile([]byte(g(`- p: { call: put_id }`, `- s: { switch: [ { c: { when: "user-id == 1", do: [] } } ] }`)), opts...)
	if err == nil || !strings.Contains(err.Error(), `key "user-id" is not a CEL identifier`) {
		t.Fatalf("got %v", err)
	}
}

func TestYAMLAliases(t *testing.T) {
	main := graph.NewEntry[NoIn, ABOut]("main")
	p := mustCompile(t, behaviour(t, main), `
graphs:
  main:
    do:
      - one: { call: put, with: &cfg { key: b, val: shared } }
      - two: { call: put, with: *cfg }
`)
	out, err := graph.Run(context.Background(), p, main, NoIn{})
	if err != nil {
		t.Fatal(err)
	}
	if out.B.V != "shared" {
		t.Errorf("b = %+v", out.B)
	}
}
