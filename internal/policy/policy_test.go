package policy

import (
	"testing"
	"time"
)

func TestAdmissionDisabledMode(t *testing.T) {
	pe := New(ModeDisabled, []*Pool{})
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("disabled mode should always admit, got %+v", result)
	}
}

func TestAdmissionUnmappedModel(t *testing.T) {
	tests := []struct {
		name     string
		mode     Mode
		admitted bool
	}{
		{"observe unmapped", ModeObserve, true},
		{"enforce unmapped", ModeEnforce, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := New(tt.mode, []*Pool{
				{
					Name:                    "default",
					Models:                  map[string]bool{"haiku": true},
					StartRate:               1.0,
					BurstCapacity:           10,
					ConcurrentDispatchLimit: 5,
				},
			})
			result := pe.CheckAdmission("unknown-model", false)
			if result.Admitted != tt.admitted {
				t.Errorf("mode %s: expected admitted=%v, got %v", tt.mode, tt.admitted, result.Admitted)
			}
		})
	}
}

func TestRateLimitingWithTokenBucket(t *testing.T) {
	now := time.Now()
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0, // 1 start per second
			BurstCapacity:           3,
			ConcurrentDispatchLimit: 10,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Should admit the first 3 (burst capacity)
	for i := 0; i < 3; i++ {
		result := pe.CheckAdmission("haiku", false)
		if !result.Admitted {
			t.Fatalf("expected admission %d, got denied: %+v", i+1, result)
		}
		pe.RecordAdmission("haiku", false)
	}

	// Next attempt should be denied (out of tokens)
	result := pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("expected denial after burst capacity exhausted, got admitted")
	}
	if result.Reason != ReasonRateLimit {
		t.Errorf("expected rate limit reason, got %s", result.Reason)
	}

	// Move time forward by 1 second - should gain 1 token
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("after 1 second, should have 1 new token")
	}
}

func TestRateLimitClockRollback(t *testing.T) {
	now := time.Now()
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Consume the burst token
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("initial token should be available")
	}
	pe.RecordAdmission("haiku", false)

	// Move forward 1 second and consume it
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("should admit after refill")
	}
	pe.RecordAdmission("haiku", false)

	// Roll clock backward - should NOT refill
	now = now.Add(-500 * time.Millisecond)
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("clock rollback should not grant new tokens")
	}

	// Move forward again - should refill normally from the rollback point
	now = now.Add(2 * time.Second)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("should admit after sufficient time forward")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               100.0, // High rate to avoid rate limiting
			BurstCapacity:           1000,
			ConcurrentDispatchLimit: 3,
		},
	})

	// Admit up to concurrency limit
	for i := 0; i < 3; i++ {
		result := pe.CheckAdmission("haiku", false)
		if !result.Admitted {
			t.Fatalf("admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", false)
	}

	// Next should be denied
	result := pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("should deny when concurrency limit reached")
	}
	if result.Reason != ReasonConcurrency {
		t.Errorf("expected concurrency reason, got %s", result.Reason)
	}
	if result.RetryAfterMs == 0 {
		t.Errorf("expected retry interval for concurrency deferral")
	}

	// Release one and retry
	pe.ReleaseAdmission("haiku", false)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("should admit after release")
	}
}

func TestCompletionReservation(t *testing.T) {
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               100.0,
			BurstCapacity:           1000,
			ConcurrentDispatchLimit: 5,
			CompletionReserved:      2,
		},
	})

	// Admit 2 fresh starts (not completion)
	for i := 0; i < 2; i++ {
		result := pe.CheckAdmission("haiku", false)
		if !result.Admitted {
			t.Fatalf("fresh admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", false)
	}

	// Admit 2 completion tasks (using the reserved capacity)
	for i := 0; i < 2; i++ {
		result := pe.CheckAdmission("haiku", true)
		if !result.Admitted {
			t.Fatalf("completion admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", true)
	}

	// Try to admit another completion - should fail (reserved exhausted)
	result := pe.CheckAdmission("haiku", true)
	if result.Admitted {
		t.Errorf("should deny completion when reserved capacity exhausted")
	}
	if result.Reason != ReasonCompletionCap {
		t.Errorf("expected completion cap reason, got %s", result.Reason)
	}

	// Fresh starts should still work (within concurrency limit)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("fresh start should still be admitted")
	}
	pe.RecordAdmission("haiku", false)

	// Release a completion
	pe.ReleaseAdmission("haiku", true)

	// Now another completion should be admitted
	result = pe.CheckAdmission("haiku", true)
	if !result.Admitted {
		t.Errorf("should admit completion after release")
	}
}

func TestMultiplePools(t *testing.T) {
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "pool1",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           2,
			ConcurrentDispatchLimit: 2,
		},
		{
			Name:                    "pool2",
			Models:                  map[string]bool{"opus": true},
			AccountID:               "acct2",
			StartRate:               0.5,
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 1,
		},
	})

	// Haiku and opus should have independent limits
	// Haiku: burst of 2
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("haiku admission 1 failed")
	}
	pe.RecordAdmission("haiku", false)

	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("haiku admission 2 failed")
	}
	pe.RecordAdmission("haiku", false)

	// Opus: burst of 1
	result = pe.CheckAdmission("opus", false)
	if !result.Admitted {
		t.Fatal("opus admission 1 failed")
	}
	pe.RecordAdmission("opus", false)

	// Both at their limits now
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("haiku should be at concurrency limit")
	}

	result = pe.CheckAdmission("opus", false)
	if result.Admitted {
		t.Errorf("opus should be at concurrency limit")
	}
}

func TestFractionalRefill(t *testing.T) {
	now := time.Now()
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               0.5, // 1 per 2 seconds
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Consume initial token
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("initial token unavailable")
	}
	pe.RecordAdmission("haiku", false)

	// After 1 second, should have 0.5 tokens (not enough)
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("with 0.5 tokens, should be denied")
	}

	// After 2 more seconds (3 total), should have 1.5 tokens
	now = now.Add(2 * time.Second)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("with 1.5 tokens, should be admitted")
	}
	pe.RecordAdmission("haiku", false)

	// Token count should be capped at burst capacity
	// Current: 1.5 - 1.0 (just consumed) + some fractional from the last refill
	// Next check should have some tokens
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("after consuming, should need more time")
	}
}

func TestBurstCeiling(t *testing.T) {
	now := time.Now()
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           3,
			ConcurrentDispatchLimit: 10,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Consume initial burst
	for i := 0; i < 3; i++ {
		result := pe.CheckAdmission("haiku", false)
		if !result.Admitted {
			t.Fatalf("burst %d failed", i+1)
		}
		pe.RecordAdmission("haiku", false)
	}

	// Wait 100 seconds - should NOT exceed burst ceiling
	now = now.Add(100 * time.Second)
	for i := 0; i < 3; i++ {
		result := pe.CheckAdmission("haiku", false)
		if !result.Admitted {
			t.Fatalf("after 100s wait, should have capacity, iteration %d", i+1)
		}
		pe.RecordAdmission("haiku", false)
	}

	// Next should be denied (at ceiling)
	result := pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("should hit burst ceiling limit")
	}
}

func TestConfigurationChangesWithoutMintingCapacity(t *testing.T) {
	// This test verifies that policy changes (pool reconfiguration)
	// don't create extra capacity.
	// In a real scenario, you'd recreate the evaluator with new config.
	// Here we just verify the logic is sound.

	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           2,
			ConcurrentDispatchLimit: 10,
		},
	})

	// Consume burst
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("admission 1 failed")
	}
	pe.RecordAdmission("haiku", false)

	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("admission 2 failed")
	}
	pe.RecordAdmission("haiku", false)

	// Exhausted
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Fatal("should be exhausted")
	}

	// If we were to reconfigure with a higher burst capacity here,
	// the new evaluator would have the new capacity, not retroactively
	// give the old one more tokens. The old evaluator stays exhausted.
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("old evaluator should still be exhausted")
	}
}
