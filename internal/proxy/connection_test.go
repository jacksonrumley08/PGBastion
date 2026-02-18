package proxy

import (
	"net"
	"testing"
	"time"
)

func TestConnectionTracker_AddRemoveCount(t *testing.T) {
	ct := NewConnectionTracker(testLogger())

	if ct.Count() != 0 {
		t.Fatalf("expected 0 connections, got %d", ct.Count())
	}

	c1, c2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	defer b1.Close()
	defer b2.Close()

	id := ct.Add(c1, b1, func() {})

	if ct.Count() != 1 {
		t.Fatalf("expected 1 connection, got %d", ct.Count())
	}

	ct.Remove(id)

	if ct.Count() != 0 {
		t.Fatalf("expected 0 connections after remove, got %d", ct.Count())
	}
}

func TestConnectionTracker_ConnectionsByBackend(t *testing.T) {
	ct := NewConnectionTracker(testLogger())

	c1, _ := net.Pipe()
	b1, _ := net.Pipe()
	c2, _ := net.Pipe()
	b2, _ := net.Pipe()
	defer c1.Close()
	defer b1.Close()
	defer c2.Close()
	defer b2.Close()

	ct.Add(c1, b1, func() {})
	ct.Add(c2, b2, func() {})

	byBackend := ct.ConnectionsByBackend()
	if len(byBackend) == 0 {
		t.Fatal("expected non-empty by_backend map")
	}

	total := 0
	for _, count := range byBackend {
		total += count
	}
	if total != 2 {
		t.Errorf("expected total 2, got %d", total)
	}
}

func TestConnectionTracker_RemoveNonexistent(t *testing.T) {
	ct := NewConnectionTracker(testLogger())
	// Should not panic.
	ct.Remove(999)
	if ct.Count() != 0 {
		t.Error("expected 0 count")
	}
}

func TestConnectionTracker_DrainAll_Empty(t *testing.T) {
	ct := NewConnectionTracker(testLogger())
	// Should return immediately, not hang.
	ct.DrainAll(1 * time.Second)
}

func TestConnectionTracker_DrainAll_CancelsConnections(t *testing.T) {
	ct := NewConnectionTracker(testLogger())

	c1, _ := net.Pipe()
	b1, _ := net.Pipe()
	defer c1.Close()
	defer b1.Close()

	cancelled := false
	ct.Add(c1, b1, func() { cancelled = true })

	ct.DrainAll(500 * time.Millisecond)

	if !cancelled {
		t.Error("expected cancel function to be called during drain")
	}
}

func TestConnectionTracker_MultipleAdd(t *testing.T) {
	ct := NewConnectionTracker(testLogger())

	ids := make([]uint64, 5)
	for i := 0; i < 5; i++ {
		c, _ := net.Pipe()
		b, _ := net.Pipe()
		ids[i] = ct.Add(c, b, func() {})
	}

	if ct.Count() != 5 {
		t.Fatalf("expected 5 connections, got %d", ct.Count())
	}

	// IDs should be unique and increasing.
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Errorf("expected monotonically increasing IDs, got %d after %d", ids[i], ids[i-1])
		}
	}
}
