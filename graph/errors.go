package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CompileError reports every problem found while compiling a graph document;
// Compile never stops at the first one.
type CompileError struct {
	Problems []Problem
}

func (e *CompileError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "graph: %d compile problem(s):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		b.WriteString(p.String())
	}
	return b.String()
}

// Problem is one compile-time finding. Module is the id of the imported
// document it is in, empty for the document given to Compile. Line and Col
// point into that document (1-based) and are 0 when the problem has no
// position.
type Problem struct {
	Module string
	Path   string // task path, e.g. "main/recall/es"
	Line   int
	Column int
	Msg    string
}

// problemf builds the Problem for a finding at path and position at.
func problemf(path string, at pos, format string, args ...any) Problem {
	return Problem{Module: at.moduleID, Path: path, Line: at.line, Column: at.col, Msg: fmt.Sprintf(format, args...)}
}

func (p Problem) String() string {
	var b strings.Builder
	if p.Module != "" {
		b.WriteString(p.Module)
		b.WriteString(":")
		if p.Line == 0 {
			b.WriteString(" ")
		}
	}
	if p.Line > 0 {
		fmt.Fprintf(&b, "%d:%d: ", p.Line, p.Column)
	}
	if p.Path != "" {
		b.WriteString(p.Path)
		b.WriteString(": ")
	}
	b.WriteString(p.Msg)
	return b.String()
}

// TaskError is the run-time failure of one task. Err is the cause.
type TaskError struct {
	Path string // task path, e.g. "search/recall/title"
	// Op is the operator of the failed call task; empty for other tasks.
	Op string
	// Middleware is the middleware that failed, when the failure is the
	// middleware's own (it returned an error, panicked, or did not call
	// Next); empty when the wrapped tasks failed.
	Middleware string
	Err        error
	// Panic and Stack are set when user code panicked: the recovered value
	// and the goroutine's stack. Panic is never nil then.
	Panic any
	Stack []byte
}

func (e *TaskError) Error() string {
	var who []string
	if e.Op != "" {
		who = append(who, e.Op)
	}
	if e.Middleware != "" {
		who = append(who, "middleware "+e.Middleware)
	}
	if len(who) > 0 {
		return fmt.Sprintf("graph: %s (%s): %v", e.Path, strings.Join(who, ", "), e.Err)
	}
	return fmt.Sprintf("graph: %s: %v", e.Path, e.Err)
}

func (e *TaskError) Unwrap() error { return e.Err }

// ErrTimeout is the cause of a task that exceeded its own timeout. It wraps
// context.DeadlineExceeded, and is also the context.Cause the task's
// operators see.
var ErrTimeout = fmt.Errorf("task timeout: %w", context.DeadlineExceeded)

// Causes that only a bug in the document or the Go code produces. They are
// not exported: nothing should branch on them, only log them.
var (
	// errNextNotCalled: a middleware returned nil without calling next, so
	// the wrapped region never ran.
	errNextNotCalled = errors.New("middleware returned without calling next")
	// errNoCase: no case of a switch matched and it has no default case.
	errNoCase = errors.New("no switch case matched (write `default: { do: [] }` to skip the switch)")
	// errUnknownEntry: Run with an Entry the Program was not compiled with.
	errUnknownEntry = errors.New("graph: entry not in program")
)

// panicError carries a recovered panic from guard to the TaskError built by
// the node that called user code.
type panicError struct {
	value any
	stack []byte
}

func (e *panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// newTaskError attributes err to a task, recording a recovered panic.
func newTaskError(path, op string, err error) *TaskError {
	te := &TaskError{Path: path, Op: op, Err: err}
	if pe, ok := err.(*panicError); ok {
		te.Panic, te.Stack = pe.value, pe.stack
	}
	return te
}
