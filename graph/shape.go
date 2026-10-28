package graph

import (
	"fmt"
	"reflect"
	"strings"
)

// shape describes how an In or Out struct maps onto the blackboard. It is
// computed once, at registration.
//
// The type must be a struct. Every exported field carries `graph:"name"`
// and binds the key name; a Maybe[T] field binds a key that may be absent.
// Where allowed (operator and middleware inputs), at most one field may
// instead carry `graph:",with"`: it receives the node's static config.
// Unexported fields are ignored and may not carry the tag. struct{} binds
// nothing.
type shape struct {
	typ    reflect.Type
	fields []field
	with   *configField // nil: takes no config
}

// configField is the `graph:",with"` field of a shape.
type configField struct {
	index []int
	typ   reflect.Type
}

// configType is the type of the node config the shape takes, or nil.
func (s *shape) configType() reflect.Type {
	if s.with == nil {
		return nil
	}
	return s.with.typ
}

type field struct {
	key   string
	index []int
	typ   reflect.Type // value type; for Maybe[T] fields this is T
	maybe bool         // a Maybe[T] field
}

const (
	tagName = "graph"
	withTag = ",with"
)

// shapeKind says which tags a shape may use.
type shapeKind int

const (
	dataOnly  shapeKind = iota // entry In/Out, operator Out
	takesWith                  // operator In, middleware In
)

func analyzeShape(t reflect.Type, kind shapeKind) (shape, error) {
	s := shape{typ: t}
	if t.Kind() != reflect.Struct {
		return s, fmt.Errorf("%v is not a struct; inputs and outputs are structs with `graph:\"key\"` fields", t)
	}
	if _, ok := maybeElem(t); ok {
		return s, fmt.Errorf("%v: Maybe can only be used as a field", t)
	}
	seen := make(map[string]bool)
	for i := range t.NumField() {
		f := t.Field(i)
		tag, ok := f.Tag.Lookup(tagName)
		if !f.IsExported() {
			if ok {
				return s, fmt.Errorf("%v.%s: graph tag on unexported field", t, f.Name)
			}
			continue
		}
		if !ok {
			return s, fmt.Errorf("%v.%s: exported field lacks a `graph:\"key\"` tag", t, f.Name)
		}
		if tag == withTag {
			switch {
			case kind != takesWith:
				return s, fmt.Errorf("%v.%s: `graph:\",with\"` is only allowed in operator inputs and middleware views", t, f.Name)
			case s.with != nil:
				return s, fmt.Errorf("%v.%s: more than one `graph:\",with\"` field", t, f.Name)
			}
			if _, isMaybe := maybeElem(f.Type); isMaybe {
				return s, fmt.Errorf("%v.%s: the `graph:\",with\"` field cannot be Maybe", t, f.Name)
			}
			s.with = &configField{index: f.Index, typ: f.Type}
			continue
		}
		if tag == "" || strings.ContainsAny(tag, ",/ ") {
			return s, fmt.Errorf("%v.%s: bad graph tag %q (want a key name)", t, f.Name, tag)
		}
		if seen[tag] {
			return s, fmt.Errorf("%v.%s: key %q bound twice", t, f.Name, tag)
		}
		seen[tag] = true
		fd := field{key: tag, index: f.Index, typ: f.Type}
		if elem, isMaybe := maybeElem(f.Type); isMaybe {
			fd.typ, fd.maybe = elem, true
		}
		s.fields = append(s.fields, fd)
	}
	return s, nil
}

// read assembles a value of the shape's type from the blackboard and the
// node config cfg (nil if the shape takes none). Keys missing from the board
// leave their field at the zero value (Maybe fields get OK == false); the
// compiler guarantees that non-Maybe fields are present.
func (s *shape) read(b *board, rename map[string]string, cfg any) any {
	v := reflect.New(s.typ).Elem()
	if s.with != nil {
		setVal(v.FieldByIndex(s.with.index), cfg)
	}
	for _, f := range s.fields {
		val, ok := b.get(keyOf(rename, f.key))
		if !ok {
			continue
		}
		fv := v.FieldByIndex(f.index)
		if f.maybe {
			setVal(fv.Field(0), val)
			fv.Field(1).SetBool(true)
		} else {
			setVal(fv, val)
		}
	}
	return v.Interface()
}

// write writes the fields of out to the blackboard. Maybe fields are written
// only when OK.
func (s *shape) write(b *board, rename map[string]string, out any) {
	v := reflect.ValueOf(out)
	for _, f := range s.fields {
		fv := v.FieldByIndex(f.index)
		if f.maybe {
			if !fv.Field(1).Bool() {
				continue
			}
			fv = fv.Field(0)
		}
		b.set(keyOf(rename, f.key), fv.Interface())
	}
}

// setVal stores val into v; a nil val (a nil interface value) leaves v at
// its zero value.
func setVal(v reflect.Value, val any) {
	if val != nil {
		v.Set(reflect.ValueOf(val))
	}
}

func keyOf(rename map[string]string, tag string) string {
	if k, ok := rename[tag]; ok {
		return k
	}
	return tag
}
