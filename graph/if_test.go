package graph_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	graph "github.com/tra4less/go-graph/graph"
)

type maybeAnswer struct {
	Answer graph.Maybe[string] `graph:"answer"`
}

func TestIfSkipsTask(t *testing.T) {
	main := graph.NewEntry[MainIn, maybeAnswer]("main")
	doc := `
graphs:
  main:
    do:
      - user: { call: load_user }
      - parse: { call: parse }
      - gen: { call: vip_answer, if: "user.Level >= 3" }
`
	for _, c := range []struct {
		uid    string
		answer string // "" when gen is skipped
	}{{"vip", "vip:a,b"}, {"bob", ""}} {
		calls, rec := recordCalls()
		p := mustCompile(t, optsOf(append(baseOps(), main)...), doc, rec...)
		out, err := graph.Run(context.Background(), p, main, MainIn{UID: c.uid, Query: "a b"})
		if err != nil || out.Answer.V != c.answer || out.Answer.OK != (c.answer != "") {
			t.Fatalf("%s: %+v, %v", c.uid, out, err)
		}
		if calls.ran("main/gen") != (c.answer != "") {
			t.Errorf("%s: gen ran = %v; ran %v", c.uid, calls.ran("main/gen"), calls.paths())
		}
	}
}

// if on a do task skips the whole group: the replacement for "skip the rest".
func TestIfSkipsGroup(t *testing.T) {
	main := graph.NewEntry[MainIn, maybeAnswer]("main")
	calls, rec := recordCalls()
	p := mustCompile(t, optsOf(append(baseOps(), main)...), `
graphs:
  main:
    do:
      - user: { call: load_user }
      - rest:
          if: "uid != 'blocked'"
          do:
            - parse: { call: parse }
            - gen: { call: vip_answer }
`, rec...)
	out, err := graph.Run(context.Background(), p, main, MainIn{UID: "blocked", Query: "q"})
	if err != nil || out.Answer.OK {
		t.Fatalf("%+v, %v", out, err)
	}
	if got := calls.paths(); !slices.Equal(got, []string{"main/user"}) {
		t.Errorf("ran %v; want only main/user, the group skipped", got)
	}
}

func TestIfCompileChecks(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"writes become maybe", g(`- user: { call: load_user, if: "uid != ''" }`, `- p: { call: parse }`, `- x: { call: plain_answer }`),
			`reads "user", which may be absent`},
		{"own output not visible", g(`- user: { call: load_user, if: "user.Level > 1" }`), `if: "user" is not a key written before this point`},
		{"typo", g(`- user: { call: load_user }`, `- p: { call: parse, if: "usr.Level > 1" }`), `"usr" is not a key written before this point`},
		{"maybe key", g(`- p: { call: parse, if: "hint == 'x'" }`), `key "hint" may be absent here`},
		{"not bool", g(`- p: { call: parse, if: "uid" }`), "condition must be bool"},
		{"empty", g(`- p: { call: parse, if: "" }`), "expected a non-empty string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := graph.Compile([]byte(c.src), compileOptions(t)...)
			var ce *graph.CompileError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q in:\n%v", c.want, err)
			}
		})
	}
	// A key written on every path stays guaranteed when the if is on a
	// different task.
	mustCompile(t, baseOptions(), g(`- user: { call: load_user }`, `- p: { call: parse, if: "user.Level > 0" }`,
		`- d: { switch: [ { has: { when: "user.Level > 0", do: [ { p2: { call: parse } } ] } }, { default: { do: [ { p3: { call: parse } } ] } } ] }`,
		`- x: { call: plain_answer }`))
}

// An if that cannot be evaluated is the task's own failure: optional
// tolerates it.
func TestIfEvaluationError(t *testing.T) {
	noUser := graph.Op("no_user", func(context.Context, struct{}) (userOut, error) { return userOut{}, nil })
	main := graph.NewEntry[NoIn, struct{}]("main")
	opts := optsOf(append(baseOps(), main, noUser)...)
	parse := `{ call: parse, if: "user.Level > 1", in: { query: q } }`
	p := mustCompile(t, opts, g(`- u: { call: no_user }`, `- q: { call: put_q }`, `- p: `+parse),
		graph.Op("put_q", func(context.Context, struct{}) (struct {
			Q string `graph:"q"`
		}, error) {
			return struct {
				Q string `graph:"q"`
			}{}, nil
		}))
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	var te *graph.TaskError
	if !errors.As(err, &te) || te.Path != "main/p" || !strings.Contains(err.Error(), `if "user.Level > 1": key "user" is a nil pointer`) {
		t.Fatalf("err %v", err)
	}

	lost := strings.Replace(parse, "{ call:", "{ optional: true, call:", 1)
	p = mustCompile(t, opts, g(`- u: { call: no_user }`, `- q: { call: put_q }`, `- p: `+lost),
		graph.Op("put_q", func(context.Context, struct{}) (struct {
			Q string `graph:"q"`
		}, error) {
			return struct {
				Q string `graph:"q"`
			}{}, nil
		}))
	// optional tolerates it: the run succeeds.
	if _, err = graph.Run(context.Background(), p, main, NoIn{}); err != nil {
		t.Fatal(err)
	}
}

// A background task evaluates its if in the background, on the snapshot.
func TestIfOnBackground(t *testing.T) {
	var ran atomic.Int32
	logs := captureLog(t)
	main := graph.NewEntry[userIn, struct{}]("main")
	p := mustCompile(t, optsOf(main,
		graph.Op("rec", func(context.Context, struct{}) (struct{}, error) {
			ran.Add(1)
			return struct{}{}, nil
		}),
		graph.Op("maybe_nil_user", func(_ context.Context, in userIn) (userOut, error) {
			if in.UID == "nil" {
				return userOut{}, nil
			}
			return userOut{User: &User{Level: 1}}, nil
		}),
	), `
graphs:
  main:
    do:
      - u: { call: maybe_nil_user }
      - bg: { call: rec, background: true, timeout: 1s, if: "uid != 'no' && user.Level > 0" }
`)
	ctx := context.Background()
	for _, uid := range []string{"yes", "no", "nil"} {
		if _, err := graph.Run(ctx, p, main, userIn{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Errorf("ran %d times, want once", ran.Load())
	}
	recs := logs.records()
	if len(recs) != 1 || recs[0].attrs["path"] != "main/bg" || !strings.Contains(recs[0].attrs["err"], "nil pointer") {
		t.Errorf("logged %v", recs)
	}
}
