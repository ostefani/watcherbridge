// Package modeljson decodes model output without exposing model text in errors.
package modeljson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Decode errors classify rejection without including model-supplied text.
var (
	ErrUnknownField = errors.New("unknown field")
	ErrMalformed    = errors.New("malformed JSON")
	ErrTrailingData = errors.New("trailing data")
)

// Decode reads one JSON value into dst, rejecting unknown fields and trailing
// data with unwrapped sentinel errors. Invalid destinations return
// [json.InvalidUnmarshalError]. On error, dst may be partially populated.
//
// Decode follows encoding/json rules for null, case-insensitive field matching,
// and duplicate keys. Callers must validate required fields and values.
func Decode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var invalid *json.InvalidUnmarshalError
		if errors.As(err, &invalid) {
			return invalid
		}
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
