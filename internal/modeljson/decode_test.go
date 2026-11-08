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
}{
	{"valid", validOutput, nil},
	{"whitespace", validOutput + " \t\r\n", nil},
	{"closing bracket", validOutput + "]", ErrTrailingData},
	{"closing brace", validOutput + "}", ErrTrailingData},
	{"second object", validOutput + "{}", ErrTrailingData},
	{"second scalar", validOutput + "1", ErrTrailingData},
	{"comma", validOutput + ",", ErrTrailingData},
	{"NUL", validOutput + "\x00", ErrTrailingData},
	{"unknown field", `{"private-model-field":7}`, ErrUnknownField},
	{"nested unknown field", `{"nested":{"private-model-field":7}}`, ErrUnknownField},
	{"malformed", `{"value":`, ErrMalformed},
	{"empty", "", ErrMalformed},
	{"whitespace only", " \t\r\n", ErrMalformed},
	{"type mismatch", `{"value":"x"}`, ErrMalformed},
	{"array", `[]`, ErrMalformed},
	{"null", `null`, nil},
}

func TestDecode(t *testing.T) {
	for _, tc := range decodeCases {
		t.Run(tc.name, func(t *testing.T) {
			var dst output
			if err := Decode([]byte(tc.raw), &dst); err != tc.want {
				t.Fatalf("Decode returned %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeInvalidDestination(t *testing.T) {
	var invalid *json.InvalidUnmarshalError
	if err := Decode([]byte(validOutput), output{}); !errors.As(err, &invalid) {
		t.Fatalf("Decode returned %v, want InvalidUnmarshalError", err)
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
