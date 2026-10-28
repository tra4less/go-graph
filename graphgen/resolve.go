package main

import (
	"fmt"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

const graphPkg = "github.com/tra4less/go-graph/graph"

// declKind is what a declaration registers.
type declKind int

const (
	opDecl         declKind = iota + 1 // graph.Op
	middlewareDecl                     // graph.Middleware
	entryDecl                          // an *graph.Entry variable, listed as is
)

// resolved is a declaration checked against the Go source.
type resolved struct {
	decl
	kind declKind
	pkg  *types.Package // package that defines the symbol
	recv *types.Named   // receiver type for methods
}

// loadTarget loads the package of the generated file plus every package the
// spec references. existing is the current content of the generated file
// (nil if there is none); it is hidden from the type checker, so a stale
// file never stops regeneration.
func loadTarget(out string, existing []byte, s *spec) (*packages.Package, map[string]*packages.Package, error) {
	dir := filepath.Dir(out)
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
		Dir:  dir,
	}
	if existing != nil {
		abs, err := filepath.Abs(out)
		if err != nil {
			return nil, nil, err
		}
		cfg.Overlay = map[string][]byte{abs: []byte(packageClause(existing))}
	}
	patterns := []string{"."}
	for _, d := range s.all() {
		if !slices.Contains(patterns, d.sym.pkg) {
			patterns = append(patterns, d.sym.pkg)
		}
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, nil, err
	}
	byPath := make(map[string]*packages.Package, len(pkgs))
	var target *packages.Package
	for _, p := range pkgs {
		byPath[p.PkgPath] = p
		if filepath.Clean(dirOf(p)) == filepath.Clean(absDir(dir)) {
			target = p
		}
	}
	if target == nil || target.Types == nil {
		return nil, nil, fmt.Errorf("cannot load the Go package in %s (the generated file must live in a package with other Go files)", dir)
	}
	return target, byPath, nil
}

// packageClause keeps only the package clause of a previously generated file.
func packageClause(src []byte) string {
	for line := range strings.Lines(string(src)) {
		if strings.HasPrefix(line, "package ") {
			return line
		}
	}
	return ""
}

func dirOf(p *packages.Package) string {
	if len(p.GoFiles) == 0 {
		return ""
	}
	return filepath.Dir(p.GoFiles[0])
}

func absDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func (s *spec) all() []decl {
	return slices.Concat(s.operators, s.middleware, s.entries)
}

// resolve finds every declared symbol and checks its signature, reporting
// all problems together.
func resolve(s *spec, target *packages.Package, byPath map[string]*packages.Package) ([]resolved, error) {
	var errs errorList
	var out []resolved
	check := func(ds []decl, kind declKind) {
		for _, d := range ds {
			r, err := resolveOne(d, kind, target, byPath)
			if err != nil {
				errs.add(s.file, d.line, d.col, "%s: %v", d.sym, err)
				continue
			}
			out = append(out, r)
		}
	}
	check(s.operators, opDecl)
	check(s.middleware, middlewareDecl)
	check(s.entries, entryDecl)
	return out, errs.err()
}

func resolveOne(d decl, kind declKind, target *packages.Package, byPath map[string]*packages.Package) (resolved, error) {
	r := resolved{decl: d, kind: kind}
	p := target
	if d.sym.pkg != target.PkgPath {
		var ok bool
		if p, ok = byPath[d.sym.pkg]; !ok || p.Types == nil {
			return r, fmt.Errorf("cannot load package %q", d.sym.pkg)
		}
		if len(p.Errors) > 0 {
			return r, fmt.Errorf("package %q has errors: %v", d.sym.pkg, p.Errors[0])
		}
	}
	r.pkg = p.Types
	foreign := p != target
	if foreign && (!token.IsExported(d.sym.name) || (d.sym.recv != "" && !token.IsExported(d.sym.recv))) {
		return r, fmt.Errorf("must be exported to be used from package %s", target.Name)
	}

	if kind == entryDecl {
		v, ok := p.Types.Scope().Lookup(d.sym.name).(*types.Var)
		if !ok {
			return r, fmt.Errorf("no package-level variable %s in %s", d.sym.name, p.PkgPath)
		}
		if !isEntry(v.Type()) {
			return r, fmt.Errorf("is %v, want *graph.Entry[In, Out]", v.Type())
		}
		return r, nil
	}

	fn, recv, err := lookupFunc(p.Types, d.sym)
	if err != nil {
		return r, err
	}
	r.recv = recv
	sig := fn.Type().(*types.Signature)
	if sig.TypeParams().Len() > 0 || (recv != nil && recv.TypeParams().Len() > 0) {
		return r, fmt.Errorf("generic functions cannot be registered")
	}
	if kind == opDecl {
		return r, checkOp(sig)
	}
	return r, checkMiddleware(sig)
}

func lookupFunc(pkg *types.Package, sym symbol) (*types.Func, *types.Named, error) {
	if sym.recv == "" {
		fn, ok := pkg.Scope().Lookup(sym.name).(*types.Func)
		if !ok {
			return nil, nil, fmt.Errorf("no function %s in %s", sym.name, pkg.Path())
		}
		return fn, nil, nil
	}
	tn, ok := pkg.Scope().Lookup(sym.recv).(*types.TypeName)
	if !ok {
		return nil, nil, fmt.Errorf("no type %s in %s", sym.recv, pkg.Path())
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a defined type", sym.recv)
	}
	var recvType types.Type = named
	if sym.ptr {
		recvType = types.NewPointer(named)
	}
	sel := types.NewMethodSet(recvType).Lookup(pkg, sym.name)
	if sel == nil {
		if !sym.ptr && types.NewMethodSet(types.NewPointer(named)).Lookup(pkg, sym.name) != nil {
			return nil, nil, fmt.Errorf("method has a pointer receiver; write (*%s).%s", sym.recv, sym.name)
		}
		return nil, nil, fmt.Errorf("type %s has no method %s", sym.recv, sym.name)
	}
	fn, ok := sel.Obj().(*types.Func)
	if !ok {
		return nil, nil, fmt.Errorf("%s.%s is not a method", sym.recv, sym.name)
	}
	return fn, named, nil
}

// checkOp accepts func(context.Context, In) (Out, error) with struct In and
// Out. A node config is a `graph:",with"` field of In, checked by
// graph.Compile.
func checkOp(sig *types.Signature) error {
	ps, rs := sig.Params(), sig.Results()
	if sig.Variadic() || ps.Len() != 2 || rs.Len() != 2 || !isContext(ps.At(0).Type()) || !isError(rs.At(1).Type()) {
		return fmt.Errorf("signature %v: want func(context.Context, In) (Out, error)", sig)
	}
	if err := wantStruct("input", ps.At(1).Type()); err != nil {
		return err
	}
	if err := wantStruct("output", rs.At(0).Type()); err != nil {
		return err
	}
	return nil
}

// checkMiddleware accepts func(context.Context, graph.Wrap[In]) error with struct In.
func checkMiddleware(sig *types.Signature) error {
	ps, rs := sig.Params(), sig.Results()
	if sig.Variadic() || ps.Len() != 2 || rs.Len() != 1 || !isContext(ps.At(0).Type()) || !isError(rs.At(0).Type()) ||
		!isNamed(ps.At(1).Type(), graphPkg, "Wrap") {
		return fmt.Errorf("signature %v: want func(context.Context, graph.Wrap[In]) error", sig)
	}
	w := types.Unalias(ps.At(1).Type()).(*types.Named)
	if err := wantStruct("middleware input", w.TypeArgs().At(0)); err != nil {
		return err
	}
	return nil
}

func wantStruct(what string, t types.Type) error {
	if _, ok := t.Underlying().(*types.Struct); !ok {
		return fmt.Errorf("%s %v is not a struct; inputs and outputs are structs with `graph:\"key\"` fields", what, t)
	}
	return nil
}

func isContext(t types.Type) bool { return isNamed(t, "context", "Context") }
func isError(t types.Type) bool   { return types.Identical(t, types.Universe.Lookup("error").Type()) }

func isEntry(t types.Type) bool {
	p, ok := t.(*types.Pointer)
	return ok && isNamed(p.Elem(), graphPkg, "Entry")
}

func isNamed(t types.Type, pkg, name string) bool {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := n.Origin().Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == pkg && obj.Name() == name
}
