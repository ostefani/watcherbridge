package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/incidentsummary"
	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

func seedVerifyBriefing(t *testing.T, s *store.Store, scope, target, eventType, ref string, at time.Time, priority, what string) string {
	t.Helper()
	ctx := context.Background()
	eventID := fmt.Sprintf("evt-%s-%s-%s-%d", scope, target, ref, at.UnixNano())
	eventKey := fmt.Sprintf("k-%s-%s-%s", scope, target, ref)
	payload := []byte(fmt.Sprintf(`{"event_id":%q,"event_key":%q,"event_type":%q,"occurred_at":"2026-10-20T12:00:05Z","cycle":7,"target":%q,"host":{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-1","identity_revision":"r1","identity_confidence":"high"}}`,
		eventID, eventKey, eventType, target, ref))
	anchor, _ := store.GroupKeyForEvent(scope, eventType, eventID, payload)
	res, err := s.PersistEventAndGroup(ctx, store.RawEvent{
		EventID: eventID, EventKey: eventKey, EventType: eventType,
		OccurredAt: at, Cycle: 1, Target: target, Payload: payload,
		ReceivedAt: at, SignatureTimestamp: at, SourceScope: scope,
	}, anchor, store.GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour, GroupingVersion: store.GroupingScoped})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectReported(ctx, scope, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeGroup(ctx, res.GroupID); err != nil {
		t.Fatal(err)
	}
	if what == "" {
		what = "reported change on " + ref
	}
	if err := s.InsertBriefing(ctx, store.Briefing{
		BriefingID: "brf-" + res.GroupID, GroupID: res.GroupID, HostAnchorKey: anchor,
		Baseline: "low", Priority: priority, PriorityReason: "test",
		WhatChanged: what, Context: "ctx " + ref, RecommendedCheck: "check",
		CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	return "brf-" + res.GroupID
}

func waitSummaryTerminal(t *testing.T, s *store.Store, id string, timeout time.Duration) store.IncidentSummary {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sum, err := s.GetIncidentSummary(ctx, id)
		if err == nil && sum.Status != "pending" && sum.Status != "running" {
			return sum
		}
		time.Sleep(50 * time.Millisecond)
	}
	sum, err := s.GetIncidentSummary(ctx, id)
	t.Fatalf("summary %s not terminal: %+v %v", id, sum, err)
	return store.IncidentSummary{}
}

func incidentWithTarget(t *testing.T, s *store.Store, target string) string {
	t.Helper()
	list, err := s.ListIncidents(context.Background(), 200, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list {
		if in.Target == target {
			return in.IncidentID
		}
	}
	t.Fatalf("no incident for target %s", target)
	return ""
}

func TestSummaryGatesVerification(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "gates.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindSourceIdentity(ctx, "lab", "ns-1", store.StableIdentityCapability); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)

	// 1. Eligibility on close: three qualified refs form, close enqueues pending.
	seedVerifyBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:g1:1", base, "low", "")
	seedVerifyBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:g1:2", base.Add(time.Minute), "medium", "")
	seedVerifyBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:g1:3", base.Add(2*time.Minute), "medium", "")
	settings := store.IncidentSettings{Window: 5 * time.Minute, Quiet: 120 * time.Second, MinRefs: 3, SummaryEnabled: true}
	rep, err := s.CorrelateIncidents(ctx, settings, base.Add(3*time.Minute))
	if err != nil || rep.Created != 1 {
		t.Fatalf("formation: %+v %v", rep, err)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	incID := list[0].IncidentID
	if _, err := s.GetIncidentSummary(ctx, incID); err == nil {
		t.Fatal("open incident must not enqueue a summary")
	}
	rep, err = s.CorrelateIncidents(ctx, settings, base.Add(3*time.Minute).Add(121*time.Second))
	if err != nil || rep.Closed != 1 {
		t.Fatalf("close: %+v %v", rep, err)
	}
	pend, err := s.GetIncidentSummary(ctx, incID)
	if err != nil || pend.Status != "pending" || pend.Floor != list[0].Floor || pend.FinalPriority != list[0].Floor {
		t.Fatalf("pending row: %+v %v", pend, err)
	}

	// 2. Worker success: medium floor + high suggestion → high, unclamped.
	fake := provider.NewFake()
	fake.SummaryHandle = func(repair bool, payload []byte, valid map[string]struct{}) (provider.RawSummary, error) {
		var snap map[string]any
		if err := json.Unmarshal(payload, &snap); err != nil {
			return provider.RawSummary{}, err
		}
		members, _ := snap["members"].([]any)
		if len(members) == 0 {
			return provider.RawSummary{}, fmt.Errorf("empty snapshot members")
		}
		alias, _ := members[0].(map[string]any)["alias"].(string)
		return provider.RawSummary{JSON: []byte(fmt.Sprintf(`{"summary":"burst of %d members","hypothesis":"likely burst","confidence":"high","priority_suggestion":"high","priority_reason":"one step raise","citations":[{"member":%q}]}`, len(members), alias)), Usage: provider.Usage{InputTokens: 10, OutputTokens: 5, HasUsage: true}}, nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- incidentsummary.Run(runCtx, incidentsummary.Deps{
			Store: s, Provider: fake,
			Redaction: redactor.Policy{Enabled: true, MaxFieldChars: 256},
			Now:       time.Now,
		})
	}()
	got := waitSummaryTerminal(t, s, incID, 15*time.Second)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker: %v", err)
	}
	if got.Status != "model" || got.FinalPriority != "high" || got.Clamped {
		t.Fatalf("resolved: %+v", got)
	}
	if got.Suggestion == nil || *got.Suggestion != "high" || got.Confidence != "high" {
		t.Fatalf("provenance: %+v", got)
	}
	if len(got.Citations) != 1 || got.InputTokens == nil || *got.InputTokens != 10 {
		t.Fatalf("citations/usage: %+v", got)
	}
	if fake.Summaries != 1 {
		t.Fatalf("sends = %d, want 1", fake.Summaries)
	}

	// 3. Overview uses resolved priority; detail carries provenance.
	cands, err := s.IncidentCandidates(ctx, base.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.IncidentID == incID && c.Floor == "high" {
			found = true
		}
	}
	if !found {
		t.Fatalf("candidates do not use resolved priority: %+v", cands)
	}
	counts, _, err := s.IncidentEffectiveCounts(ctx, base.Add(-time.Hour), time.Now().UTC())
	if err != nil || counts["high"] < 1 {
		t.Fatalf("effective counts: %+v %v", counts, err)
	}

	// 4. Excessive raise clamps one step (low floor + critical → medium, clamped).
	seedVerifyBriefing(t, s, "lab", "lan-b", "host.appeared", "mac:g2:1", base.Add(10*time.Minute), "low", "")
	seedVerifyBriefing(t, s, "lab", "lan-b", "host.appeared", "mac:g2:2", base.Add(11*time.Minute), "low", "")
	seedVerifyBriefing(t, s, "lab", "lan-b", "host.appeared", "mac:g2:3", base.Add(12*time.Minute), "low", "")
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(13*time.Minute)); err != nil {
		t.Fatal(err)
	}
	lowID := incidentWithTarget(t, s, "lan-b")
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(13*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}
	fake2 := provider.NewFake()
	fake2.SummaryHandle = func(repair bool, payload []byte, valid map[string]struct{}) (provider.RawSummary, error) {
		var snap map[string]any
		_ = json.Unmarshal(payload, &snap)
		members, _ := snap["members"].([]any)
		alias, _ := members[0].(map[string]any)["alias"].(string)
		return provider.RawSummary{JSON: []byte(fmt.Sprintf(`{"summary":"s","hypothesis":"h","confidence":"low","priority_suggestion":"critical","priority_reason":"r","citations":[{"member":%q}]}`, alias))}, nil
	}
	runCtx2, cancel2 := context.WithCancel(ctx)
	done2 := make(chan error, 1)
	go func() {
		done2 <- incidentsummary.Run(runCtx2, incidentsummary.Deps{
			Store: s, Provider: fake2,
			Redaction: redactor.Policy{Enabled: true, MaxFieldChars: 256},
			Now:       time.Now,
		})
	}()
	got2 := waitSummaryTerminal(t, s, lowID, 15*time.Second)
	cancel2()
	<-done2
	if got2.Status != "model" || got2.FinalPriority != "medium" || !got2.Clamped {
		t.Fatalf("clamp: %+v", got2)
	}

	// 5. Repair path: forged citation rejected, valid repair accepted; 2 sends share the run.
	seedVerifyBriefing(t, s, "lab", "lan-c", "host.appeared", "mac:g3:1", base.Add(20*time.Minute), "medium", "")
	seedVerifyBriefing(t, s, "lab", "lan-c", "host.appeared", "mac:g3:2", base.Add(21*time.Minute), "medium", "")
	seedVerifyBriefing(t, s, "lab", "lan-c", "host.appeared", "mac:g3:3", base.Add(22*time.Minute), "medium", "")
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(23*time.Minute)); err != nil {
		t.Fatal(err)
	}
	repID := incidentWithTarget(t, s, "lan-c")
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(23*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}
	fake3 := provider.NewFake()
	fake3.SummaryHandle = func(repair bool, payload []byte, valid map[string]struct{}) (provider.RawSummary, error) {
		if !repair {
			return provider.RawSummary{JSON: []byte(`{"summary":"s","hypothesis":"h","confidence":"medium","priority_suggestion":null,"priority_reason":"r","citations":[{"member":"M9"}]}`)}, nil
		}
		return provider.RawSummary{JSON: []byte(`{"summary":"ok","hypothesis":"ok hyp","confidence":"low","priority_suggestion":null,"priority_reason":"abstain stands","citations":[{"member":"M1"}]}`)}, nil
	}
	runCtx3, cancel3 := context.WithCancel(ctx)
	done3 := make(chan error, 1)
	go func() {
		done3 <- incidentsummary.Run(runCtx3, incidentsummary.Deps{
			Store: s, Provider: fake3,
			Redaction: redactor.Policy{Enabled: true, MaxFieldChars: 256},
			Now:       time.Now,
		})
	}()
	got3 := waitSummaryTerminal(t, s, repID, 15*time.Second)
	cancel3()
	<-done3
	if got3.Status != "model" || got3.Suggestion != nil || got3.FinalPriority != got3.Floor {
		t.Fatalf("repair outcome: %+v", got3)
	}
	if fake3.Summaries != 1 || fake3.SummaryRepairCt != 1 {
		t.Fatalf("sends: summaries=%d repairs=%d", fake3.Summaries, fake3.SummaryRepairCt)
	}

	// 6. Failure fallback preserves deterministic presentation.
	seedVerifyBriefing(t, s, "lab", "lan-d", "host.appeared", "mac:g4:1", base.Add(30*time.Minute), "high", "")
	seedVerifyBriefing(t, s, "lab", "lan-d", "host.appeared", "mac:g4:2", base.Add(31*time.Minute), "high", "")
	seedVerifyBriefing(t, s, "lab", "lan-d", "host.appeared", "mac:g4:3", base.Add(32*time.Minute), "high", "")
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(33*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fbID := incidentWithTarget(t, s, "lan-d")
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(33*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}
	fake4 := provider.NewFake(provider.FakeStep{Err: fmt.Errorf("provider down")})
	runCtx4, cancel4 := context.WithCancel(ctx)
	done4 := make(chan error, 1)
	go func() {
		done4 <- incidentsummary.Run(runCtx4, incidentsummary.Deps{
			Store: s, Provider: fake4,
			Redaction: redactor.Policy{Enabled: true, MaxFieldChars: 256},
			Now:       time.Now,
		})
	}()
	got4 := waitSummaryTerminal(t, s, fbID, 15*time.Second)
	cancel4()
	<-done4
	if got4.Status != "fallback" || got4.FinalPriority != got4.Floor {
		t.Fatalf("fallback: %+v", got4)
	}

	// 7. Snapshot sampling: 25 members → 20 sampled, floor member first, privacy tokenized.
	for i := 0; i < 25; i++ {
		pri := "low"
		if i == 7 {
			pri = "high"
		}
		seedVerifyBriefing(t, s, "lab", "lan-e", "host.appeared", fmt.Sprintf("mac:big:%d", i), base.Add(40*time.Minute).Add(time.Duration(i)*10*time.Second), pri, "seen at 192.0.2.5 from host printer-9")
	}
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	bigID := incidentWithTarget(t, s, "lan-e")
	members, omitted, err := s.SummaryMembers(ctx, bigID, 20)
	if err != nil || len(members) != 20 || omitted != 5 {
		t.Fatalf("sampling: n=%d omitted=%d %v", len(members), omitted, err)
	}
	if members[0].Priority != "high" {
		t.Fatalf("floor member not first: %+v", members[0])
	}
	inc, _ := s.GetIncident(ctx, bigID)
	raw, issued, _, aliasMap, sampled, omitted2, fail := incidentsummary.BuildSnapshot(ctx, s, inc, redactor.Policy{Enabled: true, MaxFieldChars: 256})
	if fail != "" || sampled != 20 || omitted2 != 5 || len(aliasMap) != 20 {
		t.Fatalf("snapshot: fail=%q sampled=%d omitted=%d aliases=%d", fail, sampled, omitted2, len(aliasMap))
	}
	if len(raw) > 32<<10 {
		t.Fatalf("snapshot %d bytes exceeds 32 KiB", len(raw))
	}
	// Addresses in member summaries ride durable map-owned tokens; the
	// raw form must be gone. Unknown single-label words travel inert,
	// exactly as in briefing bundles under the shared leak oracle.
	if strings.Contains(string(raw), "192.0.2.5") {
		t.Fatal("snapshot leaks raw address")
	}
	if _, ok := issued["IP_1"]; !ok {
		t.Fatalf("expected tokenized snapshot, issued=%v", issued)
	}

	// Close the large incident now so its summary pends before later keys arrive.
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(45*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}

	// 7b. Scan-CIDR targets and source scopes never cross raw: a CIDR
	// target rides the tokenized bundle anchor and the source rides a
	// per-snapshot opaque alias.
	for i, ref := range []string{"mac:cidr:1", "mac:cidr:2", "mac:cidr:3"} {
		seedVerifyBriefing(t, s, "lab", "192.0.2.0/24", "host.appeared", ref, base.Add(48*time.Minute).Add(time.Duration(i)*10*time.Second), "low", "")
	}
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(49*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cidrID := incidentWithTarget(t, s, "192.0.2.0/24")
	if _, err := s.CorrelateIncidents(ctx, settings, base.Add(49*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}
	cidrInc, _ := s.GetIncident(ctx, cidrID)
	cidrRaw, _, _, _, _, _, cidrFail := incidentsummary.BuildSnapshot(ctx, s, cidrInc, redactor.Policy{Enabled: true, MaxFieldChars: 256})
	if cidrFail != "" {
		t.Fatalf("cidr snapshot: %s", cidrFail)
	}
	if strings.Contains(string(cidrRaw), "192.0.2.0/24") || strings.Contains(string(cidrRaw), `"source_scope":"lab"`) {
		t.Fatalf("snapshot leaks raw scope: %s", cidrRaw)
	}
	if !strings.Contains(string(cidrRaw), `"source_scope":"S1"`) || !strings.Contains(string(cidrRaw), "NET_") {
		t.Fatalf("snapshot missing opaque scope/target: %s", cidrRaw)
	}

	// 8. Claim/lease on the large incident's pending summary: expiry
	// allows reclaim; stale completion loses.
	now := time.Now().UTC()
	job, claim, ok, err := s.ClaimIncidentSummary(ctx, now)
	if err != nil || !ok || job.IncidentID != bigID {
		t.Fatalf("claim big: %+v %q %v %v", job, claim, ok, err)
	}
	if err := s.ReserveSummarySnapshot(ctx, bigID, claim, string(raw), `{"M1":"x"}`, 20, 5, now); err != nil {
		t.Fatal(err)
	}
	_, claim2, ok, err := s.ClaimIncidentSummary(ctx, now.Add(61*time.Second))
	if err != nil || !ok || claim2 == claim {
		t.Fatalf("reclaim after lease: %q %v %v", claim2, ok, err)
	}
	stale := store.IncidentSummary{Status: "model", Floor: inc.Floor, FinalPriority: inc.Floor}
	committed, err := s.CompleteIncidentSummary(ctx, bigID, claim, string(raw), stale, now)
	if err != nil || committed {
		t.Fatalf("stale claim committed: %v %v", committed, err)
	}
	// The reclaimed claim retains the snapshot identity; reserving the
	// same value remains safe for a non-dispatched test setup.
	if err := s.ReserveSummarySnapshot(ctx, bigID, claim2, string(raw), `{"M1":"x"}`, 20, 5, now); err != nil {
		t.Fatal(err)
	}
	// Finish bigID under the fresh claim so the queue drains for saturation.
	finisher := store.IncidentSummary{Status: "fallback", Floor: inc.Floor, FinalPriority: inc.Floor, Error: "gates verification drain"}
	if ok2, err := s.CompleteIncidentSummary(ctx, bigID, claim2, string(raw), finisher, now); err != nil || !ok2 {
		t.Fatalf("finish big: %v %v", ok2, err)
	}

	// Drain the CIDR summary (abstain → floor stands) so saturation
	// starts from an empty queue.
	fakeCidr := provider.NewFake()
	fakeCidr.SummaryHandle = func(repair bool, payload []byte, valid map[string]struct{}) (provider.RawSummary, error) {
		var snap map[string]any
		_ = json.Unmarshal(payload, &snap)
		members, _ := snap["members"].([]any)
		alias, _ := members[0].(map[string]any)["alias"].(string)
		return provider.RawSummary{JSON: []byte(fmt.Sprintf(`{"summary":"s","hypothesis":"h","confidence":"low","priority_suggestion":null,"priority_reason":"floor stands","citations":[{"member":%q}]}`, alias))}, nil
	}
	runCtxC, cancelC := context.WithCancel(ctx)
	doneC := make(chan error, 1)
	go func() {
		doneC <- incidentsummary.Run(runCtxC, incidentsummary.Deps{
			Store: s, Provider: fakeCidr,
			Redaction: redactor.Policy{Enabled: true, MaxFieldChars: 256},
			Now:       time.Now,
		})
	}()
	gotC := waitSummaryTerminal(t, s, cidrID, 15*time.Second)
	cancelC()
	<-doneC
	if gotC.Status != "model" || gotC.Suggestion != nil || gotC.FinalPriority != gotC.Floor || gotC.Clamped {
		t.Fatalf("abstain: %+v", gotC)
	}
	if n, _ := s.SummaryOutstanding(ctx); n != 0 {
		t.Fatalf("queue not drained: %d", n)
	}

	// 9. Queue saturation through real closures (MinRefs=1): fill to 100, next close skips.
	satSettings := store.IncidentSettings{Window: 5 * time.Minute, Quiet: 120 * time.Second, MinRefs: 1, SummaryEnabled: true}
	satBase := base.Add(70 * time.Minute)
	for i := 0; i < 99; i++ {
		seedVerifyBriefing(t, s, "lab", fmt.Sprintf("lan-sat-%03d", i), "host.appeared", fmt.Sprintf("mac:sat:%d", i), satBase.Add(time.Duration(i)*time.Second), "low", "")
	}
	if _, err := s.CorrelateIncidents(ctx, satSettings, satBase.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Already-quiet incidents close in the formation pass itself, so
	// the 99 closures split across the two passes; the queue depth is
	// the assertion that matters.
	if _, err := s.CorrelateIncidents(ctx, satSettings, satBase.Add(5*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.SummaryOutstanding(ctx); n != 99 {
		t.Fatalf("outstanding = %d, want 99", n)
	}
	// One more closure reaches the 100 cap; the following one must skip.
	seedVerifyBriefing(t, s, "lab", "lan-sat-099", "host.appeared", "mac:sat:99", satBase.Add(8*time.Minute), "low", "")
	if _, err := s.CorrelateIncidents(ctx, satSettings, satBase.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CorrelateIncidents(ctx, satSettings, satBase.Add(9*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.SummaryOutstanding(ctx); n != 100 {
		t.Fatalf("outstanding = %d, want 100", n)
	}
	seedVerifyBriefing(t, s, "lab", "lan-sat-over", "host.appeared", "mac:sat:over", satBase.Add(12*time.Minute), "low", "")
	if _, err := s.CorrelateIncidents(ctx, satSettings, satBase.Add(13*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CorrelateIncidents(ctx, satSettings, satBase.Add(13*time.Minute).Add(121*time.Second)); err != nil {
		t.Fatal(err)
	}
	overID := incidentWithTarget(t, s, "lan-sat-over")
	over, err := s.GetIncidentSummary(ctx, overID)
	if err != nil || over.Status != "skipped" || over.Error != "skipped_overload" {
		t.Fatalf("over row: %+v %v", over, err)
	}

	// 10. Retention purge drains summaries before their incidents within the row budget.
	purgeNow := satBase.Add(4 * time.Hour)
	purgeSettings := store.PurgeSettings{BriefingRetention: 30 * 24 * time.Hour, SubjectRetention: 90 * 24 * time.Hour, IncidentRetention: time.Hour}
	purgeRep, err := s.PurgeOnce(ctx, purgeSettings, purgeNow)
	if err != nil {
		t.Fatal(err)
	}
	if purgeRep.Summaries < 1 {
		t.Fatalf("purge deleted no summaries: %+v", purgeRep)
	}
	if _, err := s.GetIncidentSummary(ctx, overID); err != sql.ErrNoRows {
		t.Fatalf("aged summary must drain: %v", err)
	}
	if _, err := s.GetIncident(ctx, overID); err != sql.ErrNoRows {
		t.Fatalf("aged incident must drain after its children: %v", err)
	}
}
