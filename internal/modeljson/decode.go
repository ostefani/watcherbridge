// Package modeljson decodes strict model output without exposing decoder text.
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

// Decode reads one JSON value into dst and rejects unknown fields and trailing data.
func Decode(raw []byte, dst any) error {
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
