package ratelimit

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestBackoffUntilResetThenResumes(t *testing.T) {
	l := New(time.Minute, 0)
	reset := t0.Add(10 * time.Minute)
	if got := l.Enter("o", reset, t0); !got.Equal(reset) {
		t.Fatalf("notBefore = %v, want %v", got, reset)
	}
	if b, r := l.Check("o", t0.Add(time.Minute)); !b || r {
		t.Fatalf("before reset: backedOff=%v resumed=%v", b, r)
	}
	if b, r := l.Check("o", reset); b || !r {
		t.Fatalf("at reset: backedOff=%v resumed=%v, want resumed", b, r)
	}
	if b, r := l.Check("o", reset.Add(time.Second)); b || r {
		t.Fatalf("after resume: backedOff=%v resumed=%v", b, r)
	}
	if b, _ := l.Check("other", t0); b {
		t.Fatal("unrelated owner backed off")
	}
}

func TestEnterFallsBackToInterval(t *testing.T) {
	l := New(time.Minute, 0)
	for _, reset := range []time.Time{{}, t0.Add(-time.Hour), t0} {
		if got := l.Enter("o", reset, t0); !got.Equal(t0.Add(time.Minute)) {
			t.Fatalf("reset %v: notBefore = %v", reset, got)
		}
	}
}

func TestBelowFloor(t *testing.T) {
	l := New(time.Minute, 100)
	if !l.BelowFloor(100) || !l.BelowFloor(5) || l.BelowFloor(101) {
		t.Fatal("floor comparison wrong")
	}
	if New(time.Minute, 0).BelowFloor(0) {
		t.Fatal("disabled floor must never skip")
	}
}

func TestSharedInstanceBetweenHolders(t *testing.T) {
	shared := New(time.Minute, 0)
	a, b := shared, shared
	reset := t0.Add(5 * time.Minute)
	a.Enter("o", reset, t0)
	if backed, _ := b.Check("o", t0.Add(time.Minute)); !backed {
		t.Fatal("second holder not told to skip owner")
	}
	if backed, resumed := b.Check("o", reset); backed || !resumed {
		t.Fatal("second holder should resume at reset")
	}
}
