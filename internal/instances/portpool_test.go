package instances

import (
	"context"
	"testing"
)

func TestPortPool_AllocateFreeReuse(t *testing.T) {
	d, _ := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40102)
	// Fake bind-test: all ports bindable.
	pp.tryListen = func(port int) (func() error, error) { return func() error { return nil }, nil }

	a, err := pp.Allocate(ctx, "i1")
	if err != nil || a != 40100 {
		t.Fatalf("first allocate = %d, %v; want 40100", a, err)
	}
	b, err := pp.Allocate(ctx, "i2")
	if err != nil || b != 40101 {
		t.Fatalf("second allocate = %d, %v; want 40101", b, err)
	}
	// Free the first; it should be reusable.
	if err := pp.Free(ctx, 40100); err != nil {
		t.Fatalf("Free: %v", err)
	}
	c, err := pp.Allocate(ctx, "i3")
	if err != nil || c != 40100 {
		t.Fatalf("reuse allocate = %d, %v; want 40100 reused", c, err)
	}
}

func TestPortPool_SkipsInUsePorts(t *testing.T) {
	d, _ := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40102)
	// 40100 is "in use" (bind refused); allocator must skip to 40101.
	pp.tryListen = func(port int) (func() error, error) {
		if port == 40100 {
			return nil, context.DeadlineExceeded // any error = in use
		}
		return func() error { return nil }, nil
	}
	got, err := pp.Allocate(ctx, "i1")
	if err != nil || got != 40101 {
		t.Fatalf("allocate = %d, %v; want 40101 (40100 in use)", got, err)
	}
}

func TestPortPool_Exhaustion(t *testing.T) {
	d, _ := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40101) // only 2 ports
	pp.tryListen = func(port int) (func() error, error) { return func() error { return nil }, nil }

	_, _ = pp.Allocate(ctx, "i1")
	_, _ = pp.Allocate(ctx, "i2")
	if _, err := pp.Allocate(ctx, "i3"); err == nil {
		t.Fatal("expected exhaustion error when all ports allocated")
	}
	// Free one → allocation works again.
	_ = pp.Free(ctx, 40100)
	if _, err := pp.Allocate(ctx, "i3"); err != nil {
		t.Fatalf("allocate after free should succeed: %v", err)
	}
}

func TestPortPool_FreeByInstance(t *testing.T) {
	d, _ := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(port int) (func() error, error) { return func() error { return nil }, nil }
	p, _ := pp.Allocate(ctx, "i1")
	if err := pp.FreeByInstance(ctx, "i1"); err != nil {
		t.Fatalf("FreeByInstance: %v", err)
	}
	p2, _ := pp.Allocate(ctx, "i2")
	if p2 != p {
		t.Fatalf("expected freed port %d reusable, got %d", p, p2)
	}
}
