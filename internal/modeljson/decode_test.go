package modeljson

import (
	"encoding/json"
	"errors"
	"testing"
)

type output struct {
	Value  int `json:"value"`
	Nested struct {
		Value int `json:"value"`
	} `json:"nested"`
}

const validOutput = `{"value":7}`

var decodeCases = []struct {
	name, raw string
	want      error
	wantValue int
}{
	{"valid", validOutput, nil, 7},
	{"whitespace", validOutput + " \t\r\n", nil, 7},
	{"case-insensitive key", `{"VALUE":7}`, nil, 7},
	{"duplicate scalar key", `{"value":1,"value":7}`, nil, 7},
	{"closing bracket", validOutput + "]", ErrTrailingData, 0},
	{"closing brace", validOutput + "}", ErrTrailingData, 0},
	{"second object", validOutput + "{}", ErrTrailingData, 0},
	{"second scalar", validOutput + "1", ErrTrailingData, 0},
	{"comma", validOutput + ",", ErrTrailingData, 0},
	{"NUL", validOutput + "\x00", ErrTrailingData, 0},
	{"unknown field", `{"private-model-field":7}`, ErrUnknownField, 0},
	{"nested unknown field", `{"nested":{"private-model-field":7}}`, ErrUnknownField, 0},
	{"malformed", `{"value":`, ErrMalformed, 0},
	{"empty", "", ErrMalformed, 0},
	{"whitespace only", " \t\r\n", ErrMalformed, 0},
	{"type mismatch", `{"value":"x"}`, ErrMalformed, 0},
	{"array", `[]`, ErrMalformed, 0},
	{"null", `null`, nil, 0}, // A null leaves the destination struct untouched.
}

func TestDecode(t *testing.T) {
	for _, tc := range decodeCases {
		t.Run(tc.name, func(t *testing.T) {
			var dst output
			if err := Decode([]byte(tc.raw), &dst); err != tc.want {
				t.Fatalf("Decode returned %v, want %v", err, tc.want)
			}
			if tc.want == nil && dst.Value != tc.wantValue {
				t.Fatalf("decoded value = %d, want %d", dst.Value, tc.wantValue)
			}
		})
	}
}

func TestDecodeInvalidDestination(t *testing.T) {
	for _, tc := range []struct {
		name string
		dst  any
	}{
		{"non-pointer", output{}},
		{"nil interface", nil},
		{"typed nil pointer", (*output)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, raw := range []string{validOutput, `{"value":`} {
				var invalid *json.InvalidUnmarshalError
				if err := Decode([]byte(raw), tc.dst); !errors.As(err, &invalid) {
					t.Errorf("Decode(%q) returned %v, want InvalidUnmarshalError", raw, err)
				}
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	for _, tc := range decodeCases {
		f.Add([]byte(tc.raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var dst output
		err := Decode(raw, &dst)
		valid := json.Valid(raw)
		if err == nil && !valid {
			t.Fatal("accepted invalid JSON")
		}
		if valid && err == ErrTrailingData {
			t.Fatal("valid JSON classified as trailing data")
		}
	})
}

func FuzzDecodeTrailingData(f *testing.F) {
	for _, suffix := range []string{"", " \r\n", "]", "}", "{}", "1", ",", "\x00"} {
		f.Add(suffix)
	}
	f.Fuzz(func(t *testing.T, suffix string) {
		raw := []byte(validOutput + suffix)
		var dst output
		err := Decode(raw, &dst)
		if json.Valid(raw) {
			if err != nil || dst.Value != 7 {
				t.Fatalf("valid object decoded to %+v with error %v", dst, err)
			}
		} else if err != ErrTrailingData {
			t.Fatalf("invalid suffix returned %v, want trailing-data error", err)
		}
	})
}
