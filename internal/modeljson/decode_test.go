package modeljson

import (
	"encoding/json"
	"testing"
)

type output struct {
	Value int `json:"value"`
}

func TestDecode(t *testing.T) {
	const valid = `{"value":7}`
	for _, tc := range []struct {
		name, raw string
		want      error
	}{
		{"valid", valid, nil},
		{"whitespace", valid + " \t\r\n", nil},
		{"closing bracket", valid + "]", ErrTrailingData},
		{"closing brace", valid + "}", ErrTrailingData},
		{"second object", valid + "{}", ErrTrailingData},
		{"second scalar", valid + "1", ErrTrailingData},
		{"comma", valid + ",", ErrTrailingData},
		{"NUL", valid + "\x00", ErrTrailingData},
		{"unknown field", `{"private-model-field":7}`, ErrUnknownField},
		{"malformed", `{"value":`, ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst output
			if err := Decode([]byte(tc.raw), &dst); err != tc.want {
				t.Fatalf("Decode returned %v, want %v", err, tc.want)
			}
			if tc.want == nil && dst.Value != 7 {
				t.Fatalf("decoded value = %d, want 7", dst.Value)
			}
		})
	}
}

func FuzzDecodeTrailingData(f *testing.F) {
	for _, suffix := range []string{"", " \r\n", "]", "}", "{}", "1", ",", "\x00"} {
		f.Add(suffix)
	}
	f.Fuzz(func(t *testing.T, suffix string) {
		raw := []byte(`{"value":7}` + suffix)
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
