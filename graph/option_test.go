package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	graph "github.com/tra4less/go-graph/graph"
)

func TestOverride(t *testing.T) {
	fake := graph.Override(graph.Op("load_user", func(context.Context, userIn) (userOut, error) {
		return userOut{User: &User{Name: "fake", Level: 9}}, nil
	}))
	// Overrides apply after the other options wherever they appear.
	for _, opts := range [][]graph.Option{append(baseOptions(), fake), append([]graph.Option{fake}, baseOptions()...)} {
		out, err := graph.Run(context.Background(), mustCompile(t, opts, answerDoc), Main, MainIn{UID: "bob", Query: "q"})
		if err != nil {
			t.Fatal(err)
		}
		if out.Answer != "vip:q" {
			t.Errorf("answer %q", out.Answer)
		}
	}
	bad := []struct {
		name string
		opt  graph.Option
		want string
	}{
		{"signature differs", graph.Override(graph.Op("load_user", func(context.Context, parseIn) (userOut, error) {
			return userOut{}, nil
		})), "signature differs"},
		{"unknown operator", graph.Override(graph.Op("missing", func(context.Context, struct{}) (struct{}, error) {
			return struct{}{}, nil
		})), `no operator "missing"`},
		{"entry", graph.Override(graph.NewEntry[struct{}, struct{}]("x")), "cannot replace entries"},
		{"replaced twice", graph.Override(fake, graph.Override()), "Override inside Override"},
	}
	for _, c := range bad {
		_, err := graph.Compile([]byte(answerDoc), append(baseOptions(), c.opt)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
	twice := []graph.Option{fake, fake}
	if _, err := graph.Compile([]byte(answerDoc), append(baseOptions(), twice...)...); err == nil || !strings.Contains(err.Error(), "replaced twice") {
		t.Errorf("replaced twice: %v", err)
	}
}

func TestOptionErrors(t *testing.T) {
	type mixed struct {
		A string `graph:"a"`
		B string
	}
	type badTag struct {
		A string `graph:"a,weird"`
	}
	type twoWith struct {
		A int `graph:",with"`
		B int `graph:",with"`
	}
	type withOut struct {
		A int `graph:",with"`
	}
	type optWith struct {
		A graph.Maybe[int] `graph:",with"`
	}
	type pipeTag struct {
		A string `graph:",pipe"`
	}
	cases := []struct {
		name string
		c    []graph.Option
		want string
	}{
		{"duplicate op", []graph.Option{
			graph.Op("x", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil }),
			graph.Op("x", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil }),
		}, "registered twice"},
		{"untagged field", []graph.Option{
			graph.Op("x", func(context.Context, mixed) (struct{}, error) { return struct{}{}, nil }),
		}, "lacks a"},
		{"bad tag", []graph.Option{
			graph.Op("x", func(context.Context, badTag) (struct{}, error) { return struct{}{}, nil }),
		}, "bad graph tag"},
		{"two with fields", []graph.Option{
			graph.Op("x", func(context.Context, twoWith) (struct{}, error) { return struct{}{}, nil }),
		}, "more than one"},
		{"with in output", []graph.Option{
			graph.Op("x", func(context.Context, struct{}) (withOut, error) { return withOut{}, nil }),
		}, "only allowed in operator inputs and middleware views"},
		{"with in entry", []graph.Option{graph.NewEntry[withOut, struct{}]("main")}, "only allowed in operator inputs"},
		{"Maybe with", []graph.Option{
			graph.Op("x", func(context.Context, optWith) (struct{}, error) { return struct{}{}, nil }),
		}, "cannot be Maybe"},
		{"pipe tag is gone", []graph.Option{
			graph.Op("x", func(context.Context, pipeTag) (struct{}, error) { return struct{}{}, nil }),
		}, "bad graph tag"},
		{"non-struct output", []graph.Option{
			graph.Op("x", func(context.Context, struct{}) (string, error) { return "", nil }),
		}, "is not a struct"},
		{"non-struct entry input", []graph.Option{graph.NewEntry[int, struct{}]("main")}, "is not a struct"},
		{"duplicate entry", []graph.Option{
			graph.NewEntry[struct{}, struct{}]("main"), graph.NewEntry[struct{}, struct{}]("main"),
		}, "registered twice"},
	}
	for _, c := range cases {
		// Mistakes in options are plain errors, reported before the document
		// is looked at (so a nil document is fine here).
		_, err := graph.Compile(nil, c.c...)
		var ce *graph.CompileError
		if err == nil || !strings.Contains(err.Error(), c.want) || errors.As(err, &ce) {
			t.Errorf("%s: want a plain error with %q, got %v", c.name, c.want, err)
		}
	}
}
