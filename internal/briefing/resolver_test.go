package briefing

import "testing"

func lvlPtr(l Level) *Level { return &l }

func TestResolveMatrix(t *testing.T) {
	low, medium, high, critical := Low, Medium, High, Critical
	cases := map[string]struct {
		base    Level
		sugg    *Level
		final   Level
		clamped bool
	}{
		"nil suggestion stands":           {Medium, nil, Medium, false},
		"raise one step":                  {Low, lvlPtr(medium), Medium, false},
		"raise one step to high":          {Medium, lvlPtr(high), High, false},
		"jump of two clamps to one":       {Low, lvlPtr(high), Medium, true},
		"model cannot create critical":    {High, lvlPtr(critical), High, true},
		"baseline critical stays":         {Critical, lvlPtr(low), Critical, true},
		"critical suggestion on critical": {Critical, lvlPtr(critical), Critical, false},
		"equal suggestion is not clamp":   {Medium, lvlPtr(medium), Medium, false},
		"suggestion below is clamp":       {High, lvlPtr(low), High, true},
	}
	for name, tc := range cases {
		final, clamped := Resolve(tc.base, tc.sugg)
		if final != tc.final || clamped != tc.clamped {
			t.Errorf("%s: got (%v,%v), want (%v,%v)", name, final, clamped, tc.final, tc.clamped)
		}
		if final < tc.base {
			t.Errorf("%s: final %v below baseline %v", name, final, tc.base)
		}
		if final == Critical && tc.base != Critical {
			t.Errorf("%s: model created critical", name)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for _, s := range []string{"low", "medium", "high", "critical"} {
		if _, err := ParseLevel(s); err != nil {
			t.Errorf("valid level %q rejected: %v", s, err)
		}
	}
	for _, s := range []string{"", "urgent", "LOW", "info"} {
		if _, err := ParseLevel(s); err == nil {
			t.Errorf("unknown level %q accepted", s)
		}
	}
}
