package graph

import (
	"context"
	"reflect"
)

// validator is implemented by task configs that need checks beyond YAML
// decoding; see Op.
type validator interface{ Validate() error }

// nextFunc runs the tasks wrapped by a middleware; see Wrap.Next.
type nextFunc = func(ctx context.Context) error

// Wrap is one invocation of a middleware: what it wraps and what it reads.
// New fields may be added; construct it with field names.
type Wrap[In any] struct {
	// Path is the path of the wrapped task or graph, e.g. "search/detail".
	Path string
	// Op is the operator of the wrapped call task, set only for middleware
	// added by WrapCalls; empty when a wrap: list wraps a group of tasks.
	Op string
	// In holds the keys the middleware reads, taken from the blackboard
	// before the wrapped tasks start, and its config. Same rules as an
	// operator's In.
	In In
	// Next runs the wrapped tasks. It may be called more than once (to
	// retry); each call runs them again on a fresh overlay. It must be
	// called before the middleware returns. Set by the scheduler; set it
	// yourself when calling the middleware in a test.
	Next func(ctx context.Context) error
}

type opDef struct {
	name    string
	in, out shape
	invoke  func(ctx context.Context, in any) (any, error)
}

type mwDef struct {
	name   string
	in     shape // keys read at the region's entry, and the config
	invoke func(ctx context.Context, path, op string, in any, next nextFunc) error
}

// Op registers fn as operator name.
//
// In and Out are structs whose `graph:"key"` fields declare what fn reads
// and writes; see the package doc. In may also have one `graph:",with"`
// field, of any type: it receives the task's `with:` config, decoded
// strictly at compile time (unknown fields are errors), then validated if
// the type has a method Validate() error: once per `with:` in the document
// (a subgraph inlined at several places shares its configs), and once per
// task that has no `with:`. Without such a field the operator takes no
// `with:`. fn is called concurrently by many runs and must honour ctx.
func Op[In, Out any](name string, fn func(context.Context, In) (Out, error)) Option {
	return optionFunc(func(o *options) {
		if !o.register("operator", name, o.ops[name] != nil) {
			return
		}
		in, err := analyzeShape(reflect.TypeFor[In](), takesWith)
		if err != nil {
			o.fail("operator %q input: %w", name, err)
			return
		}
		out, err := analyzeShape(reflect.TypeFor[Out](), dataOnly)
		if err != nil {
			o.fail("operator %q output: %w", name, err)
			return
		}
		o.ops[name] = &opDef{name: name, in: in, out: out, invoke: func(ctx context.Context, in any) (any, error) {
			return fn(ctx, in.(In))
		}}
	})
}

// Middleware registers fn as middleware name, usable in `wrap:` lists.
//
// In is a struct with `graph:"key"` fields, like an operator's In: the keys
// the middleware reads, taken from the blackboard before the wrapped tasks
// start. Compile checks they are set there (Maybe fields may be absent).
// Keys written by the wrapped tasks are not visible through it. In may have one
// `graph:",with"` field for the config of its `wrap:` entry. Use struct{} to
// read nothing.
//
// fn must call w.Next, before returning, when it wants the wrapped tasks to
// run; returning nil without calling it fails the task.
func Middleware[In any](name string, fn func(ctx context.Context, w Wrap[In]) error) Option {
	return optionFunc(func(o *options) {
		if !o.register("middleware", name, o.mws[name] != nil) {
			return
		}
		in, err := analyzeShape(reflect.TypeFor[In](), takesWith)
		if err != nil {
			o.fail("middleware %q input: %w", name, err)
			return
		}
		o.mws[name] = &mwDef{name: name, in: in, invoke: func(ctx context.Context, path, op string, v any, next nextFunc) error {
			return fn(ctx, Wrap[In]{Path: path, Op: op, In: v.(In), Next: next})
		}}
	})
}

func (o *options) register(what, name string, dup bool) bool {
	switch {
	case name == "":
		o.fail("empty %s name", what)
		return false
	case dup && o.inOverride:
		o.fail("Override: %s %q replaced twice", what, name)
		return false
	case dup:
		o.fail("%s %q registered twice", what, name)
		return false
	}
	return true
}
