package unpack

import (
	"fmt"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type ifNonEmptyStringUnpackerInto[T any] struct {
	base UnpackerInto[T]
}

// IfNonEmptyString is a decorator for UnpackerInto that only calls into
// the underlying unpacker if the value is a non-empty string. This can
// be of use if the default value of an argument is a string, and the
// empty string is used to denote that a value is unset.
func IfNonEmptyString[T any](base UnpackerInto[T]) UnpackerInto[T] {
	return &ifNonEmptyStringUnpackerInto[T]{
		base: base,
	}
}

func (ui *ifNonEmptyStringUnpackerInto[T]) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *T) error {
	s, ok := starlark.AsString(v)
	if !ok {
		return fmt.Errorf("got %s, want string", v.Type())
	}
	if s == "" {
		return nil
	}
	return ui.base.UnpackInto(thread, v, dst)
}

func (ui *ifNonEmptyStringUnpackerInto[T]) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	s, ok := starlark.AsString(v)
	if !ok {
		return nil, fmt.Errorf("got %s, want string", v.Type())
	}
	if s == "" {
		return starlark.String(s), nil
	}
	return ui.base.Canonicalize(thread, v)
}

func (ui *ifNonEmptyStringUnpackerInto[T]) GetConcatenationOperator() syntax.Token {
	return ui.base.GetConcatenationOperator()
}
