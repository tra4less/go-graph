package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	graph "github.com/tra4less/go-graph/graph"
)

// The answer flow of answerDoc, split over three documents: the root runs
// a graph of module "answer", which runs a graph of module "users".
const (
	importRoot = `
import:
  ans: lib/answer
graphs:
  main:
    do:
      - prep:
          run: ans.prepare
      - route:
          switch:
            - vip:
                when: "user.Level >= 3"
                do:
                  - gen:
                      call: vip_answer
            - default:
                do:
                  - gen:
                      run: ans.plain
`
	importAnswer = `
import:
  u: lib/users
graphs:
  prepare:
    do:
      - both:
          fork:
            - user:
                run: u.load
            - parse:
                call: parse
  plain:
    do:
      - a:
          run: _answer
  _answer:
    do:
      - answer:
          call: plain_answer
`
	importUsers = `
graphs:
  load:
    do:
      - user:
          call: load_user
  unused_public:
    do: []
`
)

func importOptions(extra ...graph.Option) []graph.Option {
	return append(append(baseOptions(),
		graph.Import("lib/answer", []byte(importAnswer)),
		graph.Import("lib/users", []byte(importUsers)),
	), extra...)
}

func TestImportRunsAcrossDocuments(t *testing.T) {
	calls, rec := recordCalls()
	p := mustCompile(t, importOptions(rec...), importRoot)
	for _, c := range []struct {
		uid, want, last string
	}{
		{"vip", "vip:a,b", "main/route/vip/gen"},
		{"bob", "plain:bob", "main/route/default/gen/a/answer"},
	} {
		out, err := graph.Run(context.Background(), p, Main, MainIn{UID: c.uid, Query: "a b"})
		if err != nil || out.Answer != c.want {
			t.Fatalf("%s: got %q, %v; want %q", c.uid, out.Answer, err, c.want)
		}
		if !calls.ran(c.last) {
			t.Errorf("%s: %s did not run; ran %v", c.uid, c.last, calls.paths())
		}
	}
	// Paths follow where a graph is inlined, not the document it lives in.
	if !calls.ran("main/prep/both/user/user") {
		t.Errorf("paths: %v", calls.paths())
	}
}

func TestImportEntryOfImportedGraph(t *testing.T) {
	type in struct {
		UID string `graph:"uid"`
	}
	type out struct {
		User *User `graph:"user"`
	}
	load := graph.NewEntry[in, out]("u.load")
	root := "import:\n  u: lib/users\n" // a manifest: entries only
	p := mustCompile(t, optsOf(append(baseOps(), load, graph.Import("lib/users", []byte(importUsers)))...), root)
	o, err := graph.Run(context.Background(), p, load, in{UID: "ann"})
	if err != nil || o.User.Name != "ann" {
		t.Fatalf("got %+v, %v", o.User, err)
	}
}

func TestImportProblems(t *testing.T) {
	users := graph.Import("lib/users", []byte(importUsers))
	answer := graph.Import("lib/answer", []byte(importAnswer))
	cases := []struct {
		name string
		root string
		opts []graph.Option
		want string
	}{
		{"not provided", importRoot, optsOf(answer),
			`lib/answer:3:3: import "lib/users" is not provided`},
		{"provided but never imported", importRoot, optsOf(answer, users, graph.Import("lib/extra", []byte("graphs: {}"))),
			`lib/extra: graph.Import "lib/extra" is given, but no document imports it`},
		{"problem positioned in its module", importRoot, optsOf(users, graph.Import("lib/answer", []byte(strings.Replace(importAnswer, "call: parse", "call: nope", 1)))),
			`lib/answer:11:15: main/prep/both/parse: unknown operator "nope"`},
		{"unknown alias", g(`- x: { run: zz.load }`), nil,
			`run "zz.load": the document imports nothing as "zz"`},
		{"unknown graph of module", "import:\n  u: lib/users\n" + g(`- x: { run: u.nope }`), optsOf(users),
			`run "u.nope": module "lib/users" has no graph "nope"`},
		{"private across documents", "import:\n  a: lib/answer\n" + g(`- x: { run: a._answer }`), optsOf(answer, users),
			`graph "_answer" of module "lib/answer" is private`},
		{"entry on private root graph", "graphs:\n  _main:\n    do: []\n", optsOf(graph.NewEntry[struct{}, struct{}]("_main")),
			`Entry "_main": graph "_main" is private`},
		{"unused alias", "import:\n  u: lib/users\n" + g(`- p: { call: parse }`), optsOf(users),
			`2:3: import "u" (lib/users) is never used`},
		{"alias twice", "import:\n  u: lib/users\n  v: lib/users\n" + g(`- x: { run: u.load }`), optsOf(users),
			`module "lib/users" is imported twice (as "u" and "v")`},
		{"import cycle", "import:\n  a: a\n" + g(`- r: { run: a.x }`), optsOf(
			graph.Import("a", []byte("import:\n  b: b\ngraphs:\n  x:\n    do: [ { r: { run: b.y } } ]\n")),
			graph.Import("b", []byte("import:\n  a: a\ngraphs:\n  y:\n    do: [ { r: { run: a.x } } ]\n"))),
			`import cycle: a -> b -> a`},
		{"recursion across documents", "import:\n  a: a\n" + g(`- r: { run: a.x }`), optsOf(
			graph.Import("a", []byte("graphs:\n  x:\n    do: [ { r: { run: x } } ]\n"))),
			`a:3:13: x/r: recursive graph reference: a.x -> a.x`},
		{"unused private graph of a module", "import:\n  u: lib/u\n" + g(`- r: { run: u.load }`), optsOf(
			graph.Import("lib/u", []byte(importUsers+"  _helper:\n    do: []\n"))),
			`lib/u:9:3: _helper: private graph "_helper" is never run by its document`},
		{"dot in graph name", "graphs:\n  a.b:\n    do: []\n", nil, `invalid graph name "a.b"`},
		{"bad alias", "import:\n  a.b: x\ngraphs: {}\n", nil, `invalid import alias "a.b"`},
		{"neither graphs nor import", "{}", nil, `missing field "graphs"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := append(baseOptions(), c.opts...)
			_, err := graph.Compile([]byte(c.root), opts...)
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

func TestImportOptionErrors(t *testing.T) {
	cases := []struct {
		name string
		opts []graph.Option
		want string
	}{
		{"empty id", optsOf(graph.Import("", nil)), "empty module id"},
		{"twice", optsOf(graph.Import("x", nil), graph.Import("x", nil)), `Import "x" given twice`},
		{"in Override", optsOf(graph.Override(graph.Import("x", nil))), "Override cannot contain Import"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := graph.Compile([]byte(answerDoc), append(baseOptions(), c.opts...)...)
			var ce *graph.CompileError
			if err == nil || errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want option error %q, got %v", c.want, err)
			}
		})
	}
}

// A module shared by two importers (a diamond) is parsed once: its config
// is decoded once, however many documents import it.
func TestImportDiamond(t *testing.T) {
	root := "import:\n  l: left\n  r: right\n" + g(`- a: { run: l.go }`, `- b: { run: r.go }`)
	side := "import:\n  s: shared\ngraphs:\n  go:\n    do: [ { u: { run: s.load } } ]\n"
	p := mustCompile(t, append(baseOps(), graph.NewEntry[MainIn, struct{}]("main")), root,
		graph.Import("left", []byte(side)),
		graph.Import("right", []byte(side)),
		graph.Import("shared", []byte(importUsers)),
	)
	if p == nil {
		t.Fatal("nil program")
	}
}

// A missing import is reported once, where it is declared: runs and
// entries that go through it add nothing.
func TestImportNotProvidedReportedOnce(t *testing.T) {
	type in struct {
		UID string `graph:"uid"`
	}
	root := "import:\n  u: lib/users\n" + g(`- r: { run: u.load }`)
	_, err := graph.Compile([]byte(root), append(baseOps(), graph.NewEntry[MainIn, struct{}]("main"), graph.NewEntry[in, struct{}]("u.load"))...)
	var ce *graph.CompileError
	if !errors.As(err, &ce) || len(ce.Problems) != 1 || !strings.Contains(ce.Problems[0].Msg, `import "lib/users" is not provided`) {
		t.Fatalf("want exactly the missing import, got %v", err)
	}
}
