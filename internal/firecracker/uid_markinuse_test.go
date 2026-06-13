package firecracker

import (
	"errors"
	"testing"
)

// TestUIDAllocatorMarkInUse checks that MarkInUse reserves a specific uid so a
// later Acquire never hands it out. Crash recovery uses it to re-claim the uid
// of a re-adopted pre-crash VM.
func TestUIDAllocatorMarkInUse(t *testing.T) {
	a := NewUIDAllocator(100000, 100001)

	a.MarkInUse(100000)

	// Only 100001 is free now; two acquires must exhaust the range.
	uid, _, err := a.Acquire()
	if err != nil {
		t.Fatalf("Acquire after MarkInUse: %v", err)
	}
	if uid == 100000 {
		t.Fatalf("Acquire handed out a uid marked in use: %d", uid)
	}
	if _, _, err := a.Acquire(); err == nil {
		t.Fatalf("range should be exhausted after MarkInUse + Acquire")
	} else {
		var ex *ErrUIDRangeExhausted
		if !errors.As(err, &ex) {
			t.Fatalf("want ErrUIDRangeExhausted, got %v", err)
		}
	}

	// MarkInUse is idempotent and out-of-range values are ignored (no panic).
	a.MarkInUse(100000)
	a.MarkInUse(5)
}
