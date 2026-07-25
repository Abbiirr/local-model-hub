package cluster

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/db"
)

// testRegistry opens a migrated scratch DB and returns a registry whose clock is controllable, so
// the ONLINE/STALE/UNREACHABLE state machine is testable without real sleeps.
func testRegistry(t *testing.T, interval time.Duration) (*Registry, *time.Time) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reg := NewRegistry(d.DB, interval)
	now := time.Now().UTC()
	reg.clock = func() time.Time { return now }
	return reg, &now
}

func TestUpsertAndDeriveStatus(t *testing.T) {
	interval := 10 * time.Second
	reg, now := testRegistry(t, interval)
	ctx := context.Background()

	n, err := reg.Upsert(ctx, JoinRequest{NodeID: "n1", Name: "box-a", BaseURL: "http://127.0.0.1:39922", Version: "0.1.0-dev"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if n.Status != StatusOnline {
		t.Fatalf("fresh heartbeat should be ONLINE, got %q", n.Status)
	}
	if n.Name != "box-a" || n.BaseURL != "http://127.0.0.1:39922" || n.Version != "0.1.0-dev" {
		t.Fatalf("round-trip mismatch: %+v", n)
	}

	// Advance the coordinator clock past each threshold and re-read (heartbeat age drives status).
	cases := []struct {
		age  time.Duration
		want string
	}{
		{1 * interval, StatusOnline},        // <= 2x
		{2 * interval, StatusOnline},        // == 2x boundary
		{3 * interval, StatusStale},         // 2x..4x
		{4 * interval, StatusStale},         // == 4x boundary
		{5 * interval, StatusUnreachable},   // > 4x
		{100 * interval, StatusUnreachable}, // long dead
	}
	base := *now
	for _, c := range cases {
		*now = base.Add(c.age)
		got, err := reg.Get(ctx, "n1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != c.want {
			t.Errorf("age %v: status = %q, want %q", c.age, got.Status, c.want)
		}
	}
}

func TestUpsertIdempotentRefreshesLiveness(t *testing.T) {
	interval := 10 * time.Second
	reg, now := testRegistry(t, interval)
	ctx := context.Background()

	base := *now
	if _, err := reg.Upsert(ctx, JoinRequest{NodeID: "n1", Name: "box-a", BaseURL: "http://a"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, _ := reg.Get(ctx, "n1")

	// Let it go STALE, then heartbeat again with a NEW base_url (rejoin after config change).
	*now = base.Add(3 * interval)
	if s, _ := reg.Get(ctx, "n1"); s.Status != StatusStale {
		t.Fatalf("expected STALE before re-heartbeat, got %q", s.Status)
	}
	if _, err := reg.Upsert(ctx, JoinRequest{NodeID: "n1", Name: "box-a", BaseURL: "http://a-new"}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _ := reg.Get(ctx, "n1")
	if got.Status != StatusOnline {
		t.Errorf("heartbeat should refresh to ONLINE, got %q", got.Status)
	}
	if got.BaseURL != "http://a-new" {
		t.Errorf("rejoin should refresh base_url, got %q", got.BaseURL)
	}
	if got.FirstJoined != first.FirstJoined {
		t.Errorf("first_joined must be preserved across heartbeats: %q vs %q", got.FirstJoined, first.FirstJoined)
	}

	// Idempotent: still exactly one row.
	nodes, _ := reg.List(ctx)
	if len(nodes) != 1 {
		t.Errorf("expected 1 node after repeated upsert, got %d", len(nodes))
	}
}

// TestSoftStateSurvivesCoordinatorRestart proves the registry is rebuilt-from-DB soft state: a new
// Registry over the SAME db still knows the node (identity persists), reports it by its persisted
// last_seen (UNREACHABLE if stale), and returns it to ONLINE on the next heartbeat — with no
// special rebuild code (cluster-plan §2.4, §7).
func TestSoftStateSurvivesCoordinatorRestart(t *testing.T) {
	interval := 10 * time.Second
	reg, now := testRegistry(t, interval)
	ctx := context.Background()
	base := *now
	if _, err := reg.Upsert(ctx, JoinRequest{NodeID: "n1", Name: "box-a", BaseURL: "http://a"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Simulate a coordinator restart: a brand-new Registry over the same underlying DB, and time
	// has moved well past the heartbeat window (coordinator was down, no heartbeats arrived).
	reg2 := NewRegistry(reg.db, interval)
	later := base.Add(5 * interval)
	reg2.clock = func() time.Time { return later }
	got, err := reg2.Get(ctx, "n1")
	if err != nil {
		t.Fatalf("post-restart get: %v", err) // identity must persist
	}
	if got.Status != StatusUnreachable {
		t.Errorf("known-but-not-yet-heartbeated node should be UNREACHABLE, got %q", got.Status)
	}
	// Next heartbeat rebuilds liveness.
	if _, err := reg2.Upsert(ctx, JoinRequest{NodeID: "n1", Name: "box-a", BaseURL: "http://a"}); err != nil {
		t.Fatalf("post-restart heartbeat: %v", err)
	}
	if got, _ := reg2.Get(ctx, "n1"); got.Status != StatusOnline {
		t.Errorf("registry should rebuild to ONLINE from next heartbeat, got %q", got.Status)
	}
}

func TestRemoveAndNotFound(t *testing.T) {
	reg, _ := testRegistry(t, 10*time.Second)
	ctx := context.Background()
	if _, err := reg.Upsert(ctx, JoinRequest{NodeID: "n1", Name: "a", BaseURL: "http://a"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := reg.Remove(ctx, "n1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := reg.Remove(ctx, "n1"); err != ErrNodeNotFound {
		t.Errorf("second remove should be ErrNodeNotFound, got %v", err)
	}
	if _, err := reg.Get(ctx, "missing"); err != ErrNodeNotFound {
		t.Errorf("get missing should be ErrNodeNotFound, got %v", err)
	}
}

func TestUpsertRejectsIncompletePayload(t *testing.T) {
	reg, _ := testRegistry(t, 10*time.Second)
	ctx := context.Background()
	for _, req := range []JoinRequest{
		{Name: "a", BaseURL: "http://a"},   // no node_id
		{NodeID: "n1", BaseURL: "http://a"}, // no name
		{NodeID: "n1", Name: "a"},           // no base_url
	} {
		if _, err := reg.Upsert(ctx, req); err == nil {
			t.Errorf("expected error for incomplete payload %+v", req)
		}
	}
}

func TestLoadOrCreateNodeIDStable(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	id1, err := LoadOrCreateNodeID(ctx, d.DB)
	if err != nil || id1 == "" {
		t.Fatalf("first mint: id=%q err=%v", id1, err)
	}
	id2, err := LoadOrCreateNodeID(ctx, d.DB)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if id1 != id2 {
		t.Errorf("node id must be stable across calls: %q vs %q", id1, id2)
	}
}
