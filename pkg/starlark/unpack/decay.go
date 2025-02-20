package unpack

import (
	"go.starlark.net/starlark"
)

type decayUnpackerInto[T any] struct {
	UnpackerInto[T]
}

// Decay the unpacked value to one of type any. This is useful when used
// in combination with Or(), allowing unpacking to yield values that are
// otherwise incompatible with each other.
func Decay[T any](base UnpackerInto[T]) UnpackerInto[any] {
	return decayUnpackerInto[T]{
		UnpackerInto: base,
	}
}

func (ui decayUnpackerInto[T]) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *any) error {
	var tmp T
	err := ui.UnpackerInto.UnpackInto(thread, v, &tmp)
	if err != nil {
		return err
	}
	*dst = tmp
	return nil
}
