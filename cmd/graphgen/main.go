// Command graphgen generates go-graph registrations from an operators.yaml,
// so operators can be declared in YAML instead of hand-written
// option lists.
//
// Go cannot look functions up by name at run time, so the YAML is turned into
// Go code at build time (like stringer or mockgen). Typical use, next to the
// operators' package:
//
//	//go:generate go run github.com/tra4less/go-graph/cmd/graphgen -in operators.yaml -out operators_gen.go
//
// operators.yaml:
//
//	operators:
//	  parse_query: { package: github.com/tra4less/go-graph/examples/search, func: ParseQuery }          # package-level function
//	  es_search:   { package: github.com/tra4less/go-graph/examples/search, func: (*Recall).ESSearch }  # method: *Recall becomes a field of Receivers
//	  rerank:      { package: github.com/x/rank, func: Rerank }
//	middleware:
//	  timing:      { package: github.com/tra4less/go-graph/examples/search, func: (*Observer).Timing }
//	entries:
//	  - { package: github.com/tra4less/go-graph/examples/search, var: QA }   # a *graph.Entry[In, Out] variable
//
// package is always the full import path, also for symbols in the package
// of the generated file (they are then referenced unqualified). YAML anchors
// keep repeated paths short:
//
//	x-qa: &qa github.com/tra4less/go-graph/examples/search
//	operators:
//	  parse_query: { package: *qa, func: ParseQuery }
//
// Every symbol is resolved and type-checked: operators must be
// func(ctx, In) (Out, error) with struct In and Out; middleware
// func(ctx, graph.Wrap[In]) error with struct In. A task config is a
// `graph:",with"` field of In. All problems are reported at once, with
// positions in operators.yaml.
//
// Method receivers are passed in a generated Receivers struct, so
// dependencies stay injected and adding one never changes the signature:
//
//	type Receivers struct {
//		Recall   *Recall   // es_search
//		Observer *Observer // timing
//	}
//
//	// Options fails on nil pointer receivers, listing every one.
//	func Options(r Receivers) ([]graph.Option, error)
//
// The returned options are grouped by kind (entries, operators, middleware)
// in declaration order, and imports follow goimports: the standard library
// first, then the rest sorted by path.
//
// With -check, graphgen writes nothing and fails if the file on disk is not
// what it would generate; run it in CI to catch a stale file.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	in := flag.String("in", "operators.yaml", "operators declaration file")
	out := flag.String("out", "operators_gen.go", "generated Go file, in the package that uses the components")
	check := flag.Bool("check", false, "fail if -out is not up to date instead of writing it")
	flag.Parse()
	if err := run(*in, *out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "graphgen:", err)
		os.Exit(1)
	}
}

var errStale = errors.New("generated file is out of date; run go generate")

func run(in, out string, check bool) error {
	src, err := generate(in, out)
	if err != nil {
		return err
	}
	if check {
		old, err := os.ReadFile(out)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("%s does not exist: %w", out, errStale)
		case err != nil:
			return err
		case !bytes.Equal(old, src):
			return fmt.Errorf("%s: %w", out, errStale)
		}
		return nil
	}
	return os.WriteFile(out, src, 0o644)
}

// generate returns the Go source graphgen would write to out.
func generate(in, out string) ([]byte, error) {
	existing, err := os.ReadFile(out)
	if errors.Is(err, os.ErrNotExist) {
		existing, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	return generateWith(in, out, existing)
}

// generateWith is generate with the current content of out given, so tests
// need not write into the source tree.
func generateWith(in, out string, existing []byte) ([]byte, error) {
	raw, err := os.ReadFile(in)
	if err != nil {
		return nil, err
	}
	s, err := parseSpec(in, raw)
	if err != nil {
		return nil, err
	}
	target, byPath, err := loadTarget(out, existing, s)
	if err != nil {
		return nil, err
	}
	rs, err := resolve(s, target, byPath)
	if err != nil {
		return nil, err
	}
	return emit(s, target.Types, rs)
}
