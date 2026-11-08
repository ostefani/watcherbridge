// Package modeljson decodes model output without exposing model text in errors.
package modeljson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

// Decode errors classify rejection without including model-supplied text.
var (
	ErrUnknownField = errors.New("unknown field")
	ErrMalformed    = errors.New("malformed JSON")
	ErrTrailingData = errors.New("trailing data")
)

// Decode reads one JSON value into dst, rejecting unknown fields and trailing
// data with unwrapped sentinel errors. Decode checks dst before reading raw
// and returns a [*json.InvalidUnmarshalError] for an invalid destination.
// On a decoding error, dst may be partially populated.
//
// Decode follows encoding/json rules for null and case-insensitive field
// matching. Later duplicate keys replace or merge into earlier values,
// depending on the destination type. Callers must validate required fields
// and values.
func Decode(raw []byte, dst any) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return &json.InvalidUnmarshalError{Type: reflect.TypeOf(dst)}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// encoding/json has no typed unknown-field error.
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return ErrUnknownField
		}
		return ErrMalformed
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrTrailingData
	}
	return nil
}
