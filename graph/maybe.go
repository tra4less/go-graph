package graph

import "reflect"

// Maybe is a value that may be absent.
//
// As an input field it reads a key that is not guaranteed to have been
// written on every path: by an optional task, by only some switch cases, by a
// Maybe output, or a Maybe field of an Entry's input. OK reports whether the
// key was written. As an output field, OK == false means "do not write".
type Maybe[T any] struct {
	V  T
	OK bool
}

// Some returns a present Maybe.
func Some[T any](v T) Maybe[T] { return Maybe[T]{V: v, OK: true} }

// maybeTypes lets analyzeShape recognise Maybe[T] without knowing T.
func (Maybe[T]) maybeTypes() (self, elem reflect.Type) {
	return reflect.TypeFor[Maybe[T]](), reflect.TypeFor[T]()
}

type maybeValue interface {
	maybeTypes() (self, elem reflect.Type)
}

var maybeValueType = reflect.TypeFor[maybeValue]()

// maybeElem reports whether t is exactly Maybe[X] and returns X. Types that merely
// embed a Maybe (and so inherit maybeTypes) are not Maybe.
func maybeElem(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() != reflect.Struct || !t.Implements(maybeValueType) {
		return nil, false
	}
	self, elem := reflect.Zero(t).Interface().(maybeValue).maybeTypes()
	if self != t {
		return nil, false
	}
	return elem, true
}
