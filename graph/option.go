package graph

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Option is what Compile is built from: operators (Op), middleware
// (Middleware), entries (*Entry), test doubles (Override), the WrapCalls
// setting, and imported documents (Import).
//
// All but Import come from code: build them once at startup and pass the
// same values to every Compile. Import carries document content, which
// changes on hot reload: build it from the documents being compiled,
//
//	prog, err := graph.Compile(root, append(slices.Clip(codeOpts), imports...)...)
//
// Only this package implements Option.
type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) { f(o) }

// options is the symbol table Compile resolves document names against:
// name to typed definition, with the struct tags already analyzed. It is
// built anew by every Compile and never escapes it.
type options struct {
	ops            map[string]*opDef
	mws            map[string]*mwDef
	entries        map[string]*entryDef // by graph name
	imports        map[string][]byte    // documents by module id
	overrides      []Option
	wrapCalls      []string // middleware around every call task, outermost first
	wrapCallsGiven bool
	inOverride     bool // applying the options of an Override
	errs           []error
}

// buildOptions applies opts, then the overrides they contain. Its errors are
// mistakes in Go code, not in a document.
func buildOptions(opts []Option) (*options, error) {
	o := newOptions()
	for _, opt := range opts {
		if opt == nil {
			o.errs = append(o.errs, errors.New("graph: nil Option"))
			continue
		}
		opt.apply(o)
	}
	o.applyOverrides()
	for _, name := range o.wrapCalls {
		if o.mws[name] == nil {
			o.fail("WrapCalls: no middleware %q", name)
		}
	}
	if len(o.errs) > 0 {
		return nil, errors.Join(o.errs...)
	}
	return o, nil
}

func (o *options) fail(format string, args ...any) {
	o.errs = append(o.errs, fmt.Errorf("graph: "+format, args...))
}

func newOptions() *options {
	return &options{
		ops:     make(map[string]*opDef),
		mws:     make(map[string]*mwDef),
		entries: make(map[string]*entryDef),
		imports: make(map[string][]byte),
	}
}

// applyOverrides replaces operators and middleware with the ones given to
// Override, wherever Override appeared among the options.
func (o *options) applyOverrides() {
	if len(o.overrides) == 0 {
		return
	}
	repl := newOptions()
	repl.inOverride = true
	for _, opt := range o.overrides {
		if opt == nil {
			o.fail("nil Option in Override")
			continue
		}
		opt.apply(repl)
	}
	o.errs = append(o.errs, repl.errs...)
	for _, name := range slices.Sorted(maps.Keys(repl.ops)) {
		op, old := repl.ops[name], o.ops[name]
		switch {
		case old == nil:
			o.fail("Override: no operator %q", name)
		case old.in.typ != op.in.typ || old.out.typ != op.out.typ:
			o.fail("Override: operator %q signature differs", name)
		default:
			o.ops[name] = op
		}
	}
	for _, name := range slices.Sorted(maps.Keys(repl.mws)) {
		mw, old := repl.mws[name], o.mws[name]
		switch {
		case old == nil:
			o.fail("Override: no middleware %q", name)
		case old.in.typ != mw.in.typ:
			o.fail("Override: middleware %q signature differs", name)
		default:
			o.mws[name] = mw
		}
	}
}

// Override replaces operators or middleware registered by other options,
// typically with test doubles. Each replacement must exist under the same
// name with identical In and Out types. Overrides apply after all other
// options, wherever they appear; replacing a name twice is an error.
func Override(opts ...Option) Option {
	return optionFunc(func(o *options) {
		if o.inOverride {
			o.fail("Override inside Override")
			return
		}
		o.overrides = append(o.overrides, opts...)
	})
}

// Import provides the graph document src under module id, for documents
// that name id in their import: list:
//
//	import:
//	  mw: subexg/midway        # alias: module id
//	graphs:
//	  search:
//	    do:
//	      - m:
//	          run: mw.default  # graph default of module subexg/midway
//
// id is opaque to Compile, which only matches it against import: lists; a
// path or a config-center key both work, but avoid ':', which separates the
// id from the line in Problem.String. An imported document has the same
// form as the one given to Compile and may import further modules from the
// same set of Import options. Its graphs whose names start with _ are
// private to it. Each id is given once, and every id given must be imported
// by some document; problems in src are reported with id as their Module.
//
// Unlike the other options, Import carries document content: on hot
// reload, build it from the new content. Compile reads src each time it is
// given the option; do not modify src afterwards.
func Import(id string, src []byte) Option {
	return optionFunc(func(o *options) {
		switch _, dup := o.imports[id]; {
		case o.inOverride:
			o.fail("Override cannot contain Import")
		case id == "":
			o.fail("Import with an empty module id")
		case dup:
			o.fail("Import %q given twice", id)
		default:
			o.imports[id] = src
		}
	})
}

// WrapCalls puts the named middleware around every call task of the
// document, mws[0] outermost, as if each call were written
// `wrap: [mws...]`. It is how a Program gets observability (spans, metrics,
// logs) without touching the document; Wrap.Path and Wrap.Op identify the
// task.
//
// The middleware runs inside the task's timeout and overlay and directly
// around the operator, with the same rules as `wrap:`: its In is checked at
// every call task, its `graph:",with"` field (if any) gets the zero config,
// and it can retry or reject but not turn a failure into success. Give
// WrapCalls at most once, outside Override; the middleware are registered
// with Middleware as usual (and may be replaced by Override).
func WrapCalls(mws ...string) Option {
	return optionFunc(func(o *options) {
		switch {
		case o.inOverride:
			o.fail("Override cannot contain WrapCalls")
			return
		case o.wrapCallsGiven:
			o.fail("WrapCalls given twice")
			return
		}
		o.wrapCallsGiven = true
		seen := make(map[string]bool, len(mws))
		for _, name := range mws {
			if seen[name] {
				o.fail("WrapCalls: middleware %q listed twice", name)
			}
			seen[name] = true
		}
		o.wrapCalls = mws
	})
}
