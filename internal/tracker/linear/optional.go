package linear

import (
	"encoding/json"
	"errors"
	"reflect"
)

// Optional is an input field that is left alone, set to a value, or
// cleared. Its zero value is unset, which a request omits (the field
// carries `json:",omitzero"`); Null is sent as JSON null.
type Optional[T any] struct {
	state optionalState
	value T
}

type optionalState uint8

const (
	unset optionalState = iota
	set
	null
)

// Set is an Optional holding v.
func Set[T any](v T) Optional[T] { return Optional[T]{state: set, value: v} }

// Null is an Optional that clears the field.
func Null[T any]() Optional[T] { return Optional[T]{state: null} }

// IsSet reports a value, IsNull a clear; neither is an unset field.
func (o Optional[T]) IsSet() bool  { return o.state == set }
func (o Optional[T]) IsNull() bool { return o.state == null }

// Value is the value Set gave, or T's zero value.
func (o Optional[T]) Value() T { return o.value }

// IsZero reports an unset field, which omitzero leaves out.
func (o Optional[T]) IsZero() bool { return o.state == unset }

// MarshalJSON writes the value or null. A Set nil slice is written as
// an empty list, never as null, so Set(nil) cannot clear a field by
// accident. An unset field reaching it (a struct field without
// omitzero) fails the encoding rather than clear the field.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	switch o.state {
	case set:
		if v := reflect.ValueOf(o.value); v.Kind() == reflect.Slice && v.IsNil() {
			return []byte("[]"), nil
		}
		return json.Marshal(o.value)
	case null:
		return []byte("null"), nil
	case unset:
	}
	return nil, errors.New("linear: an unset Optional was encoded; its field needs omitzero")
}
