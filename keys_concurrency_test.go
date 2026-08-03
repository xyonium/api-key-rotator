package main

import (
	"context"
	"testing"
	"time"
)

// TestKeyPool_SetMaxConcurrent_UnlimitedByDefault: with no SetMaxConcurrent
// call, TryAcquire always succeeds (unlimited), Release is a safe no-op, and
// Current ignores slots entirely (legacy behavior preserved).
func TestKeyPool_SetMaxConcurrent_UnlimitedByDefault(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetCredits(0, 100)
	p.SetCredits(1, 100)
	for i := 0; i < 5; i++ {
		if !p.TryAcquire(0) {
			t.Fatalf("TryAcquire(0) attempt %d = false, want true (unlimited)", i)
		}
	}
	p.Release(0) // must not panic or go negative
	p.Release(0)
	if got := p.inFlight[0]; got != 0 {
		t.Fatalf("inFlight[0] = %d, want 0 (unlimited pools do not count)", got)
	}
}

// TestKeyPool_TryAcquire_RespectsCap: cap 1 -> first acquire true, second on
// the same key false; after Release it succeeds again.
func TestKeyPool_TryAcquire_RespectsCap(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetMaxConcurrent(1)
	if !p.TryAcquire(0) {
		t.Fatal("first TryAcquire = false, want true")
	}
	if p.TryAcquire(0) {
		t.Fatal("second TryAcquire = true, want false (cap reached)")
	}
	p.Release(0)
	if !p.TryAcquire(0) {
		t.Fatal("TryAcquire after Release = false, want true")
	}
}

// TestKeyPool_CurrentPrefersFreeSlot: equal credits, cap 1, key0 held ->
// Current returns key1 (the one with a free slot). After release, key0 again.
func TestKeyPool_CurrentPrefersFreeSlot(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetMaxConcurrent(1)
	p.SetCredits(0, 100)
	p.SetCredits(1, 100)
	if !p.TryAcquire(0) {
		t.Fatal("setup: TryAcquire(0) failed")
	}
	if i, _ := p.Current(); i != 1 {
		t.Fatalf("Current() = %d, want 1 (key0 slot full)", i)
	}
	p.Release(0)
	if i, _ := p.Current(); i != 0 {
		t.Fatalf("Current() after release = %d, want 0 (slot free again)", i)
	}
}

// TestKeyPool_Current_AllSlotsFullStillReturnsKey: cap 1, both keys acquired ->
// Current still returns a key (>=0) so the rotator sees SATURATED (queue/reject),
// not EXHAUSTED (503). AnyUsable stays true.
func TestKeyPool_Current_AllSlotsFullStillReturnsKey(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetMaxConcurrent(1)
	p.SetCredits(0, 100)
	p.SetCredits(1, 100)
	p.TryAcquire(0)
	p.TryAcquire(1)
	if i, _ := p.Current(); i < 0 {
		t.Fatalf("Current() = %d, want >= 0 (saturated, not exhausted)", i)
	}
	if !p.AnyUsable() {
		t.Fatal("AnyUsable() = false, want true (keys are usable, just busy)")
	}
}

// TestKeyPool_Current_SaturatedBelowStopIsExhausted: all keys below the stop
// threshold (with free slots) -> Current == -1 (exhausted, not saturated).
func TestKeyPool_Current_SaturatedBelowStopIsExhausted(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b"})
	p.SetThresholds(10, 2)
	p.SetMaxConcurrent(1)
	p.SetCredits(0, 1)
	p.SetCredits(1, 0)
	if i, _ := p.Current(); i != -1 {
		t.Fatalf("Current() = %d, want -1 (all below stop = exhausted)", i)
	}
}

// TestKeyPool_Release_InvalidIndexNoLeak: Release on -1, out-of-range, and a
// double-release must not panic nor corrupt inFlight.
func TestKeyPool_Release_InvalidIndexNoLeak(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetMaxConcurrent(1)
	p.TryAcquire(0)
	p.Release(-1)  // no-op
	p.Release(99)  // no-op
	p.Release(0)   // real release
	p.Release(0)   // double release: guarded, stays 0
	if got := p.inFlight[0]; got != 0 {
		t.Fatalf("inFlight[0] = %d, want 0 (no leak, no negative)", got)
	}
	if !p.TryAcquire(0) {
		t.Fatal("TryAcquire after releases = false, want true")
	}
}

func TestKeyPool_WaitForSlot_SlotFreeImmediate(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetMaxConcurrent(1)
	if !p.WaitForSlot(context.Background(), 50*time.Millisecond) {
		t.Fatal("WaitForSlot = false, want true (slot free immediately)")
	}
}

func TestKeyPool_WaitForSlot_TimeoutWhenSaturated(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetMaxConcurrent(1)
	p.TryAcquire(0) // saturate the only slot
	start := time.Now()
	if p.WaitForSlot(context.Background(), 30*time.Millisecond) {
		t.Fatal("WaitForSlot = true, want false (saturated, timed out)")
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("WaitForSlot returned after %v, want it to wait ~30ms", elapsed)
	}
}

func TestKeyPool_WaitForSlot_WakesOnRelease(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetMaxConcurrent(1)
	p.TryAcquire(0) // saturate
	go func() {
		time.Sleep(20 * time.Millisecond)
		p.Release(0)
	}()
	if !p.WaitForSlot(context.Background(), 1*time.Second) {
		t.Fatal("WaitForSlot = false, want true (woken by Release)")
	}
}

func TestKeyPool_WaitForSlot_ContextCancel(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	p.SetMaxConcurrent(1)
	p.TryAcquire(0) // saturate
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	if p.WaitForSlot(ctx, 5*time.Second) {
		t.Fatal("WaitForSlot = true, want false (ctx canceled)")
	}
}

func TestKeyPool_WaitForSlot_UnlimitedAlwaysTrue(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"}) // no SetMaxConcurrent -> unlimited
	if !p.WaitForSlot(context.Background(), 10*time.Millisecond) {
		t.Fatal("WaitForSlot = false, want true (unlimited)")
	}
}
