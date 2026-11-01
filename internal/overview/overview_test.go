package overview

import (
	"testing"
	"time"
)

func TestVerdictMatrix(t *testing.T) {
	now := time.Now()
	mk := func(id, pri string) Item {
		return Item{ID: id, Priority: pri, CreatedAt: now}
	}
	for _, tc := range []struct {
		name      string
		briefings []Item
		incidents []Item
		want      string
		wantHead  string
	}{
		{"empty quiet", nil, nil, VerdictQuiet, "Nothing needs review"},
		{"low only quiet", []Item{mk("a", "low")}, nil, VerdictQuiet, "Nothing needs review"},
		{"unknown priority quiet", []Item{mk("a", "bogus")}, nil, VerdictQuiet, "Nothing needs review"},
		{"medium review", []Item{mk("a", "medium")}, nil, VerdictReview, "1 things to review"},
		{"high act", []Item{mk("a", "medium"), mk("b", "high")}, nil, VerdictAct, "2 need a check now"},
		{"critical act", []Item{mk("a", "critical")}, nil, VerdictAct, "1 need a check now"},
		{"flagged low floors review", []Item{{ID: "a", Priority: "low", CreatedAt: now, Flagged: true}}, nil, VerdictReview, "1 things to review"},
		{"flagged high stays review", []Item{{ID: "a", Priority: "high", CreatedAt: now, Flagged: true}}, nil, VerdictReview, "1 things to review"},
		{"acknowledged medium quiet", []Item{{ID: "a", Priority: "medium", CreatedAt: now, Acknowledged: true}}, nil, VerdictQuiet, "Nothing needs review"},
		{"snoozed high quiet", []Item{{ID: "a", Priority: "high", CreatedAt: now, SnoozedUntil: now.Add(time.Hour)}}, nil, VerdictQuiet, "Nothing needs review"},
		{"expired snooze attention", []Item{{ID: "a", Priority: "high", CreatedAt: now, SnoozedUntil: now.Add(-time.Hour)}}, nil, VerdictAct, "1 need a check now"},
		{"incident medium review", nil, []Item{mk("i", "medium")}, VerdictReview, "1 things to review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Compute(tc.briefings, tc.incidents, nil, 0, 24*time.Hour, time.Time{}, Coverage{}, now)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", got.Verdict, tc.want)
			}
			if got.Headline != tc.wantHead {
				t.Fatalf("headline = %q, want %q", got.Headline, tc.wantHead)
			}
		})
	}
}

func TestAttentionOrdering(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	items := []Item{
		{ID: "b-low", Priority: "low", CreatedAt: now},
		{ID: "b-med-old", Priority: "medium", CreatedAt: old},
		{ID: "b-med-new", Priority: "medium", CreatedAt: now},
		{ID: "b-high", Priority: "high", CreatedAt: old},
		{ID: "flag-low", Priority: "low", CreatedAt: old, Flagged: true},
		{ID: "ack-high", Priority: "high", CreatedAt: now, Acknowledged: true},
		{ID: "snooze-high", Priority: "high", CreatedAt: now, SnoozedUntil: now.Add(time.Hour)},
	}
	got := Compute(items, []Item{{ID: "inc-med", Priority: "medium", CreatedAt: now}}, nil, 0, 24*time.Hour, time.Time{}, Coverage{}, now)
	var ids []string
	for _, a := range got.Attention {
		ids = append(ids, a.ID)
	}
	want := []string{"flag-low", "b-high", "inc-med", "b-med-new", "b-med-old"}
	if len(ids) != len(want) {
		t.Fatalf("attention = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("attention = %v, want %v", ids, want)
		}
	}
	// Incident-before-briefing and ID tiebreakers.
	tie := []Item{
		{ID: "b", Priority: "medium", CreatedAt: now},
		{ID: "a", Priority: "medium", CreatedAt: now},
	}
	got = Compute(tie, []Item{{ID: "i", Priority: "medium", CreatedAt: now}}, nil, 0, 24*time.Hour, time.Time{}, Coverage{}, now)
	if got.Attention[0].ID != "i" || got.Attention[1].ID != "a" || got.Attention[2].ID != "b" {
		t.Fatalf("tiebreak order = %v", got.Attention)
	}
}

func TestDigestZeroFilled(t *testing.T) {
	now := time.Now()
	got := Compute(nil, nil, map[string]int{"port_closed": 2}, 5, 24*time.Hour, now, Coverage{}, now)
	if got.Digest["port_closed"] != 2 || got.Digest["other"] != 0 || len(got.Digest) != len(DigestKinds) {
		t.Fatalf("digest = %v", got.Digest)
	}
	if got.Receipts != 5 || got.Window != 24*time.Hour || !got.LastChange.Equal(now) {
		t.Fatalf("receipts/window/lastChange = %d/%v/%v", got.Receipts, got.Window, got.LastChange)
	}
}

func TestValidateWindow(t *testing.T) {
	ret := 30 * 24 * time.Hour
	if _, err := ValidateWindow(24*time.Hour, 168*time.Hour, ret); err != nil {
		t.Fatalf("valid window rejected: %v", err)
	}
	for _, tc := range []struct {
		name        string
		window, max time.Duration
	}{
		{"zero window", 0, 168 * time.Hour},
		{"negative window", -time.Hour, 168 * time.Hour},
		{"window over max", 200 * time.Hour, 168 * time.Hour},
		{"max over retention", 24 * time.Hour, 31 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateWindow(tc.window, tc.max, ret); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAssessCoverage(t *testing.T) {
	now := time.Now()
	ok := AssessCoverage(CoverageInput{AsOf: now})
	if ok.Status != CoverageOK {
		t.Fatalf("empty input = %q, want ok", ok.Status)
	}
	// Missing heartbeat capability alone never implies blind.
	noH1 := AssessCoverage(CoverageInput{
		Sources: []SourceInput{{Scope: "s", LastReportAge: 72 * time.Hour}},
		AsOf:    now,
	})
	if noH1.Status == CoverageBlind {
		t.Fatal("silence without heartbeat capability must not be blind")
	}
	if noH1.Sources[0].Liveness != LivenessUnknown {
		t.Fatalf("liveness = %q, want unknown", noH1.Sources[0].Liveness)
	}
	// Stale heartbeat with capability is blind.
	blind := AssessCoverage(CoverageInput{
		Sources: []SourceInput{{Scope: "s", HeartbeatCapable: true, HeartbeatStale: true}},
		AsOf:    now,
	})
	if blind.Status != CoverageBlind {
		t.Fatalf("stale heartbeat = %q, want blind", blind.Status)
	}
	// Each degraded trigger on its own.
	for _, tc := range []struct {
		name string
		in   CoverageInput
	}{
		{"fallback ratio", CoverageInput{FallbackRatio: 0.6, FallbackBriefings: 4}},
		{"redaction blocked", CoverageInput{RedactionBlocked: true}},
		{"parked groups", CoverageInput{ParkedGroups: 1}},
		{"forward errors", CoverageInput{ForwardErrors: 2}},
		{"writer saturated", CoverageInput{WriterSaturated: true}},
		{"delivery gap", CoverageInput{Sources: []SourceInput{{Scope: "s", DeliveryGap: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.AsOf = now
			if got := AssessCoverage(tc.in); got.Status != CoverageDegraded {
				t.Fatalf("status = %q, want degraded", got.Status)
			}
		})
	}
	// Fallback ratio needs weight: 1 of 2 is not a signal.
	thin := AssessCoverage(CoverageInput{FallbackRatio: 1.0, FallbackBriefings: 2, AsOf: now})
	if thin.Status != CoverageOK {
		t.Fatalf("thin fallback = %q, want ok", thin.Status)
	}
	// Blind outranks degraded.
	both := AssessCoverage(CoverageInput{
		Sources:      []SourceInput{{Scope: "s", HeartbeatCapable: true, HeartbeatStale: true}},
		ParkedGroups: 1, AsOf: now,
	})
	if both.Status != CoverageBlind {
		t.Fatalf("blind+degraded = %q, want blind", both.Status)
	}
}

func TestExitCodeFor(t *testing.T) {
	if ExitCodeFor(VerdictQuiet) != 0 || ExitCodeFor(VerdictReview) != 1 || ExitCodeFor(VerdictAct) != 2 {
		t.Fatal("verdict exit mapping wrong")
	}
}

func BenchmarkCompute(b *testing.B) {
	now := time.Now()
	items := make([]Item, 0, CandidateCap)
	for i := 0; i < CandidateCap; i++ {
		pri := PriorityLow
		switch i % 4 {
		case 1:
			pri = PriorityMedium
		case 2:
			pri = PriorityHigh
		case 3:
			pri = PriorityCritical
		}
		items = append(items, Item{ID: string(rune('a'+i%26)) + string(rune('0'+i%10)), Priority: pri, CreatedAt: now})
	}
	digest := map[string]int{"port_closed": 10, "other": 490}
	cov := Coverage{Status: CoverageOK}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Compute(items, nil, digest, 500, 24*time.Hour, now, cov, now)
	}
}
