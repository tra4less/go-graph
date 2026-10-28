package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	graph "github.com/tra4less/go-graph/graph"
	amodel "github.com/tra4less/go-graph/graph/testdata/clash/a/model"
	bmodel "github.com/tra4less/go-graph/graph/testdata/clash/b/model"
)

func condDoc(when string) string {
	return "graphs:\n  main:\n    do:\n      - s: { switch: [ { c: { when: " + `"` + when + `"` + ", do: [] } } ] }\n"
}

// Two Go types with the same package name and type name get the same CEL
// name; one would stand for the other. Keys reaching either are rejected
// with the reason, also through struct fields, and only those keys.
func TestConditionTypeNameClash(t *testing.T) {
	type in struct {
		A *amodel.Info  `graph:"a"`
		B *bmodel.Info  `graph:"b"`
		H bmodel.Holder `graph:"h"`
		N int           `graph:"n"`
	}
	type onlyA struct {
		A *amodel.Info `graph:"a"`
	}
	type withFunc struct {
		F func() `graph:"f"`
	}
	cases := []struct {
		name string
		e    graph.Option
		when string
		want string // "" for success
	}{
		{"right type rejected", graph.NewEntry[in, struct{}]("main"), "a.Level == 5", "model.Info is both"},
		{"wrong type rejected", graph.NewEntry[in, struct{}]("main"), "a.Level == 'x'", "model.Info is both"},
		{"through a field", graph.NewEntry[in, struct{}]("main"), "h.Info.Level == 'x'", `key "h" cannot be used`},
		{"other keys unaffected", graph.NewEntry[in, struct{}]("main"), "n > 1", ""},
		{"no clash in scope", graph.NewEntry[onlyA, struct{}]("main"), "a.Level == 5", ""},
		{"unusable type", graph.NewEntry[withFunc, struct{}]("main"), "f == f", `key "f" is func()`},
	}
	for _, c := range cases {
		_, err := graph.Compile([]byte(condDoc(c.when)), c.e)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
}

// A comprehension variable may share its name with a key that is absent.
func TestConditionComprehensionShadowsMaybeKey(t *testing.T) {
	type in struct {
		LS   []int            `graph:"ls"`
		Hint graph.Maybe[int] `graph:"hint"`
	}
	type out struct {
		Hit graph.Maybe[string] `graph:"hit"`
	}
	e := graph.NewEntry[in, out]("main")
	p, err := graph.Compile([]byte(`
graphs:
  main:
    do:
      - s:
          switch:
            - big: { when: "ls.exists(hint, hint > 1)", do: [ { w: { call: hit } } ] }
`), e, graph.Op("hit", func(context.Context, struct{}) (out, error) { return out{Hit: graph.Some("y")}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	got, err := graph.Run(context.Background(), p, e, in{LS: []int{0, 2}})
	if err != nil || got.Hit.V != "y" {
		t.Errorf("got %+v, %v", got, err)
	}
	// The key itself is still refused.
	_, err = graph.Compile([]byte(condDoc("hint > 1")), e, graph.Op("hit", func(context.Context, struct{}) (out, error) { return out{}, nil }))
	var ce *graph.CompileError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), `key "hint" may be absent here`) {
		t.Errorf("want maybe-key error, got %v", err)
	}
}

func TestConditionOnNilPointer(t *testing.T) {
	main := graph.NewEntry[NoIn, struct{}]("main")
	cat := optsOf(main, graph.Op("no_user", func(context.Context, struct{}) (userOut, error) {
		return userOut{}, nil
	}))
	p := mustCompile(t, cat, `
graphs:
  main:
    do:
      - u: { call: no_user }
      - s: { switch: [ { vip: { when: "user.Level > 3", do: [] } } ] }
`)
	_, err := graph.Run(context.Background(), p, main, NoIn{})
	var ne *graph.TaskError
	if !errors.As(err, &ne) || ne.Path != "main/s/vip" || !strings.Contains(err.Error(), `when "user.Level > 3": key "user" is a nil pointer`) {
		t.Fatalf("err %v", err)
	}
}
