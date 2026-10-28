package graph

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// conditionEnv type-checks the conditions (`if:` and `when:`) at one point
// of a graph. Only the keys guaranteed to be set there are declared, so every other name fails
// as undeclared; explain turns that into the reason (the key may be absent,
// or its type cannot be used). The CEL environment is built on first use,
// and compiled conditions are kept: inlined subgraphs share environments.
type conditionEnv struct {
	def      map[string]reflect.Type
	maybe    map[string]bool
	unusable map[string]string // guaranteed keys left undeclared, with the reason
	nonIdent []string          // guaranteed keys that are not CEL identifiers
	env      *cel.Env
	err      error
	compiled map[string]compiledCondition // by expression
}

type compiledCondition struct {
	c   *condition
	err error
}

func newConditionEnv(def map[string]reflect.Type, maybe map[string]bool) *conditionEnv {
	return &conditionEnv{def: def, maybe: maybe, unusable: make(map[string]string), compiled: make(map[string]compiledCondition)}
}

func (e *conditionEnv) buildEnv() error {
	if e.env != nil || e.err != nil {
		return e.err
	}
	type decl struct {
		name    string
		t       *cel.Type
		natives []reflect.Type // named struct types to register
		objects []string       // CEL names of every struct type the key reaches
	}
	var decls []decl
	// ext.NativeTypes names a struct type by the last element of its package
	// path and its type name, so two Go types can get the same CEL name;
	// one would silently stand for the other. owners finds such clashes.
	owners := make(map[string][]reflect.Type)
	for _, name := range slices.Sorted(maps.Keys(e.def)) {
		if !celIdent.MatchString(name) || celReserved[name] {
			e.nonIdent = append(e.nonIdent, name) // cannot be referenced at all
			continue
		}
		d := decl{name: name}
		t, ok := celType(e.def[name], func(st reflect.Type) { d.natives = append(d.natives, st) })
		if !ok {
			e.unusable[name] = fmt.Sprintf("key %q is %v, which conditions cannot use", name, e.def[name])
			continue
		}
		d.t = t
		walkObjects(e.def[name], func(st reflect.Type) {
			cn := objectName(st)
			d.objects = append(d.objects, cn)
			if !slices.Contains(owners[cn], st) {
				owners[cn] = append(owners[cn], st)
			}
		})
		decls = append(decls, d)
	}
	var opts []cel.EnvOption
	var natives []any
	seen := make(map[reflect.Type]bool)
	for _, d := range decls {
		if i := slices.IndexFunc(d.objects, func(cn string) bool { return len(owners[cn]) > 1 }); i >= 0 {
			ts := owners[d.objects[i]]
			e.unusable[d.name] = fmt.Sprintf("key %q cannot be used in conditions: CEL names Go types by package name and type name only, and %s is both %s and %s; rename one of the types",
				d.name, d.objects[i], fullName(ts[0]), fullName(ts[1]))
			continue
		}
		opts = append(opts, cel.Variable(d.name, d.t))
		for _, st := range d.natives {
			if !seen[st] {
				seen[st] = true
				natives = append(natives, st)
			}
		}
	}
	if len(natives) > 0 {
		opts = append(opts, ext.NativeTypes(append(natives, ext.ParseStructTags(true))...))
	}
	e.env, e.err = cel.NewEnv(opts...)
	return e.err
}

// compile checks expr: it must be boolean and reference only keys that are
// guaranteed to be set, so evaluation can never miss a variable.
func (e *conditionEnv) compile(expr string) (*condition, error) {
	if r, ok := e.compiled[expr]; ok {
		return r.c, r.err
	}
	c, err := e.compileUncached(expr)
	e.compiled[expr] = compiledCondition{c, err}
	return c, err
}

func (e *conditionEnv) compileUncached(expr string) (*condition, error) {
	if err := e.buildEnv(); err != nil {
		return nil, err
	}
	ast, iss := e.env.Compile(expr)
	if err := iss.Err(); err != nil {
		return nil, e.explain(iss)
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("condition must be bool, got %v", ast.OutputType())
	}
	c := &condition{src: expr}
	for _, ref := range ast.NativeRep().ReferenceMap() {
		// Declared variables; the rest are functions, types, constants and
		// comprehension variables, which may shadow a key name.
		if _, isVar := e.def[ref.Name]; isVar && e.unusable[ref.Name] == "" && !slices.Contains(c.vars, ref.Name) {
			c.vars = append(c.vars, ref.Name)
		}
	}
	slices.Sort(c.vars)
	prg, err := e.env.Program(ast, cel.EvalOptions(cel.OptOptimize))
	if err != nil {
		return nil, err
	}
	c.prg = prg
	return c, nil
}

// explain replaces CEL's "undeclared reference" to a key by why the key is
// not declared.
func (e *conditionEnv) explain(iss *cel.Issues) error {
	for _, ce := range iss.Errors() {
		m := undeclaredRef.FindStringSubmatch(ce.Message)
		if m == nil {
			continue
		}
		name, _, _ := strings.Cut(m[1], ".")
		if r, ok := e.unusable[name]; ok {
			return errors.New(r)
		}
		if e.maybe[name] {
			return fmt.Errorf("key %q may be absent here; conditions can only use keys that are guaranteed to be set", name)
		}
		err := fmt.Errorf("%q is not a key written before this point (a task's if cannot see the task's own writes)", name)
		if i := slices.IndexFunc(e.nonIdent, func(k string) bool { return strings.HasPrefix(k, name) }); i >= 0 {
			err = fmt.Errorf("%w; key %q is not a CEL identifier, so conditions cannot use it", err, e.nonIdent[i])
		}
		return err
	}
	return iss.Err()
}

var undeclaredRef = regexp.MustCompile(`undeclared reference to '([^']*)'`)

// walkObjects calls f for every struct type reachable from t the way
// ext.NativeTypes registers them: through pointers, slices, arrays and map
// values, and through every field of a struct.
func walkObjects(t reflect.Type, f func(reflect.Type)) {
	seen := make(map[reflect.Type]bool)
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(t.Elem())
		case reflect.Struct:
			if seen[t] {
				return
			}
			seen[t] = true
			f(t)
			for i := range t.NumField() {
				walk(t.Field(i).Type)
			}
		}
	}
	walk(t)
}

// objectName is the CEL name ext.NativeTypes gives struct type t.
func objectName(t reflect.Type) string {
	pkg := t.PkgPath()
	return pkg[strings.LastIndex(pkg, "/")+1:] + "." + t.Name()
}

func fullName(t reflect.Type) string { return t.PkgPath() + "." + t.Name() }

// condition is a compiled `if:` or `when:`. eval's errors do not name the
// expression; callers say which one failed.
type condition struct {
	src  string
	prg  cel.Program
	vars []string // keys referenced by the expression, all guaranteed set
}

func (c *condition) eval(b *board) (bool, error) {
	act := make(map[string]any, len(c.vars))
	for _, name := range c.vars {
		v, _ := b.get(name)
		if isNilPointer(v) {
			// CEL has no null for object types; say so instead of failing
			// deep inside the evaluator.
			return false, fmt.Errorf("key %q is a nil pointer", name)
		}
		act[name] = v
	}
	out, _, err := c.prg.Eval(act)
	if err != nil {
		return false, err
	}
	v, ok := out.Value().(bool)
	if !ok {
		return false, errNotBool
	}
	return v, nil
}

var errNotBool = errors.New("result is not a bool")

func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

var (
	celIdent    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	celReserved = map[string]bool{
		"as": true, "break": true, "const": true, "continue": true, "else": true, "false": true,
		"for": true, "function": true, "if": true, "import": true, "in": true, "let": true,
		"loop": true, "package": true, "namespace": true, "null": true, "return": true,
		"true": true, "var": true, "void": true, "while": true,
	}
	durationType  = reflect.TypeFor[time.Duration]()
	timestampType = reflect.TypeFor[time.Time]()
)

// celType maps a Go type to its CEL type, following ext.NativeTypes. Named
// struct types are reported through native so they can be registered.
// Types CEL cannot represent (funcs, chans, anonymous structs) report false
// and the key is simply not usable in conditions.
func celType(t reflect.Type, native func(reflect.Type)) (*cel.Type, bool) {
	switch t.Kind() {
	case reflect.Bool:
		return cel.BoolType, true
	case reflect.Float32, reflect.Float64:
		return cel.DoubleType, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if t == durationType {
			return cel.DurationType, true
		}
		return cel.IntType, true
	case reflect.String:
		return cel.StringType, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return cel.UintType, true
	case reflect.Array, reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return cel.BytesType, true
		}
		et, ok := celType(t.Elem(), native)
		if !ok {
			return nil, false
		}
		return cel.ListType(et), true
	case reflect.Map:
		kt, ok := celType(t.Key(), native)
		if !ok {
			return nil, false
		}
		et, ok := celType(t.Elem(), native)
		if !ok {
			return nil, false
		}
		return cel.MapType(kt, et), true
	case reflect.Struct:
		if t == timestampType {
			return cel.TimestampType, true
		}
		if t.Name() == "" {
			return nil, false
		}
		native(t)
		return cel.ObjectType(objectName(t)), true
	case reflect.Pointer:
		return celType(t.Elem(), native)
	case reflect.Interface:
		return cel.DynType, true
	}
	return nil, false
}

// conditionEnv returns the environment for the conditions of a switch at
// flow f. A subgraph inlined at many places mostly sees the same flow, so
// environments, and the conditions compiled in them, are shared by
// signature: the guaranteed keys with their types, and the maybe keys.
func (c *compiler) conditionEnv(f flow) *conditionEnv {
	def := make(map[string]reflect.Type, len(f.def))
	var sig strings.Builder
	for _, k := range slices.Sorted(maps.Keys(f.def)) {
		if d, ok := c.types[k]; ok {
			def[k] = d.typ
			fmt.Fprintf(&sig, "%s=%d;", k, c.typeID(d.typ))
		}
	}
	sig.WriteByte('|')
	for _, k := range slices.Sorted(maps.Keys(f.maybe)) {
		sig.WriteString(k)
		sig.WriteByte(';')
	}
	if e, ok := c.envs[sig.String()]; ok {
		return e
	}
	e := newConditionEnv(def, maps.Clone(f.maybe))
	c.envs[sig.String()] = e
	return e
}

// typeID numbers Go types: their names are not unique, so they cannot key
// a signature.
func (c *compiler) typeID(t reflect.Type) int {
	id, ok := c.typeIDs[t]
	if !ok {
		id = len(c.typeIDs)
		c.typeIDs[t] = id
	}
	return id
}
