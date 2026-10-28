package graph

import (
	"bytes"
	"errors"
	"io"
	"reflect"

	"gopkg.in/yaml.v3"
)

// configKey identifies one decoding: a `with:` node and the config type it
// decodes into.
type configKey struct {
	n *yaml.Node
	t reflect.Type
}

type decodedConfig struct {
	v   any
	err error
}

// config returns decodeConfig(n, t), decoded once per config in the document
// however often its subgraph is inlined. Configs absent from the document
// are decoded every time: each task gets its own zero value.
func (c *compiler) config(n *yaml.Node, t reflect.Type) (any, error) {
	if n == nil {
		return decodeConfig(n, t)
	}
	k := configKey{n, t}
	d, ok := c.cfgs[k]
	if !ok {
		d.v, d.err = decodeConfig(n, t)
		c.cfgs[k] = d
	}
	return d.v, d.err
}

// decodeConfig strictly decodes n (unknown fields are errors) into a new value
// of type t, then runs Validate if implemented. t == nil means the component
// takes no config. A missing config decodes as the zero value, or as a
// pointer to one when t is a pointer type, and is still validated.
func decodeConfig(n *yaml.Node, t reflect.Type) (any, error) {
	if t == nil {
		if n != nil {
			return nil, errors.New("takes no config (its input has no `graph:\",with\"` field)")
		}
		return nil, nil
	}
	base := t
	if t.Kind() == reflect.Pointer {
		base = t.Elem()
	}
	ptr := reflect.New(base)
	// Decoding and Validate run user code (UnmarshalYAML, Validate) on input
	// that may arrive by hot reload: a panic must reject the document, not
	// crash the process.
	err := guard(func() error {
		if n != nil {
			// yaml.Node.Decode cannot reject unknown fields; a strict Decoder
			// needs bytes. Aliases were expanded by the parser, so n stands alone.
			buf, err := yaml.Marshal(n)
			if err != nil {
				return err
			}
			dec := yaml.NewDecoder(bytes.NewReader(buf))
			dec.KnownFields(true)
			if err := dec.Decode(ptr.Interface()); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		}
		// *base has the methods of both receivers, so this finds Validate either way.
		if v, ok := ptr.Interface().(validator); ok {
			return v.Validate()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if t.Kind() == reflect.Pointer {
		return ptr.Interface(), nil
	}
	return ptr.Elem().Interface(), nil
}
