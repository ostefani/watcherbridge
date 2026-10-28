package briefing

import "fmt"

// Level is the briefing priority. The model may only raise, one step, and
// never past High; only a deterministic baseline rule creates Critical.
type Level int

const (
	Low Level = iota
	Medium
	High
	Critical
)

func (l Level) String() string {
	switch l {
	case Low:
		return "low"
	case Medium:
		return "medium"
	case High:
		return "high"
	case Critical:
		return "critical"
	}
	return "unknown"
}

// ParseLevel rejects unknown strings: a typo in a rule table or a model
// suggestion must fail loudly, never store silently.
func ParseLevel(s string) (Level, error) {
	switch s {
	case "low":
		return Low, nil
	case "medium":
		return Medium, nil
	case "high":
		return High, nil
	case "critical":
		return Critical, nil
	}
	return Low, fmt.Errorf("unknown priority level %q", s)
}

// Resolve applies the raise-only rule to a baseline and an optional model
// suggestion (nil = the model abstains and the baseline stands). A larger
// jump is clamped to one step; raises are capped at High. clamped reports
// whether the suggestion differs from the final priority for any of those
// reasons, including a suggestion below the baseline.
func Resolve(baseline Level, suggestion *Level) (final Level, clamped bool) {
	if suggestion == nil {
		return baseline, false
	}
	want := *suggestion
	if want <= baseline {
		return baseline, want != baseline
	}
	final = baseline + 1
	if final > High {
		final = High // the model never creates critical
	}
	if final > want {
		final = want
	}
	return final, final != want
}
