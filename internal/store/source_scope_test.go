package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Step 30.1 scope binding: one database serves one configured source.
// Binding refuses a database that already holds another deployment's
// scoped receipts instead of mixing their subjects, and the binding
// is durable across restart. Matching attributes in different scopes
// never associate: same reference, separate profiles, separate
// groups.

func TestBindSourceScopeAcceptsOwnScopeAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.BindSourceScope(ctx, "lab"); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	body := producerPayload("evt-scope-1", "k", "host.appeared",
		`{"host_id":"mac:aa:bb:cc:dd:ee:01","identity_source":"mac","identity_confidence":"high"}`)
	persistReported(t, s, "lab", "evt-scope-1", "k", "host.appeared", body, at)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Same scope after restart: durable, no refusal.
	if err := s.BindSourceScope(ctx, "lab"); err != nil {
		t.Fatalf("own scope must rebind after restart: %v", err)
	}
}

func TestBindSourceScopeRefusesForeignScope(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	body := producerPayload("evt-scope-2", "k", "host.appeared",
		`{"host_id":"mac:aa:bb:cc:dd:ee:02","identity_source":"mac","identity_confidence":"high"}`)
	persistReported(t, s, "lab", "evt-scope-2", "k", "host.appeared", body, at)
	if err := s.BindSourceScope(ctx, "lab"); err != nil {
		t.Fatalf("own scope must bind: %v", err)
	}
	if err := s.BindSourceScope(ctx, "other-lab"); err == nil {
		t.Fatal("foreign scope must be refused, never mixed")
	}
	if err := s.BindSourceScope(ctx, ""); err == nil {
		t.Fatal("empty scope must be refused")
	}
}

// The same upstream reference reported in two scopes yields two
// profiles and two groups: matching attributes never create a
// cross-source association.
func TestSameReferenceNeverAssociatesAcrossScopes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	host := `{"host_id":"mac:aa:bb:cc:dd:ee:03","identity_source":"mac","identity_confidence":"high"}`
	persistReported(t, s, "lab-a", "evt-xa-1", "k", "host.appeared", producerPayload("evt-xa-1", "k", "host.appeared", host), at)
	persistReported(t, s, "lab-b", "evt-xb-1", "k", "host.appeared", producerPayload("evt-xb-1", "k", "host.appeared", host), at)
	if _, err := s.ProjectReported(ctx, "lab-a", 64); err != nil {
		t.Fatal(err)
	}
	a := subjectProfileID(t, s, "lab-a", SubjectReportedDevice, "", "mac:aa:bb:cc:dd:ee:03")
	b := subjectProfileID(t, s, "lab-b", SubjectReportedDevice, "", "mac:aa:bb:cc:dd:ee:03")
	if a == b {
		t.Fatalf("scopes must not share a profile: %q", a)
	}
}
