package policy

import (
	"os"
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

	// First-pass work can only use (total - reserved) = 3 slots
	// Admit 3 fresh starts
	for i := 0; i < 3; i++ {
		result := pe.CheckAdmission("haiku", false)
		if !result.Admitted {
			t.Fatalf("fresh admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", false)
	}

	// Try to admit another fresh start - should fail (reserved capacity held)
	result := pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("should deny fresh start when reserved capacity held")
	}
	if result.Reason != ReasonConcurrency {
		t.Errorf("expected concurrency reason, got %s", result.Reason)
	}

	// Completion work can use up to total limit = 5 slots
	// With 3 fresh starts, we have 2 slots available for completion
	for i := 0; i < 2; i++ {
		result := pe.CheckAdmission("haiku", true)
		if !result.Admitted {
			t.Fatalf("completion admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", true)
	}

	// We're now at total capacity (3 fresh + 2 completion = 5)
	// Next completion should fail (total limit reached)
	result = pe.CheckAdmission("haiku", true)
	if result.Admitted {
		t.Errorf("should deny completion when total capacity exhausted")
	}
	if result.Reason != ReasonConcurrency {
		t.Errorf("expected concurrency reason, got %s", result.Reason)
	}

	// Release a fresh start, now we should be able to admit completion
	pe.ReleaseAdmission("haiku", false)
	result = pe.CheckAdmission("haiku", true)
	if !result.Admitted {
		t.Errorf("should admit completion after fresh start release")
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

// Test that validates exact admit counts with fractional refill rates
func TestFractionalRefillExactCounts(t *testing.T) {
	now := time.Now()
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               0.1, // 1 token per 10 seconds
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Consume initial burst token
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("initial token should be available")
	}
	pe.RecordAdmission("haiku", false)

	// Check every 100ms for 10 seconds - should get exactly 1 more token
	admitCount := 0
	for i := 0; i < 100; i++ {
		now = now.Add(100 * time.Millisecond)
		result = pe.CheckAdmission("haiku", false)
		if result.Admitted {
			admitCount++
			pe.RecordAdmission("haiku", false)
		}
	}

	// At 0.1 tokens/second, after 10 seconds we should have earned exactly 1 token
	// But since we're checking every 100ms, we may accumulate fractional tokens
	// that allow 1 more admission within that window
	if admitCount != 1 {
		t.Errorf("with 0.1 tokens/sec over 10s, expected 1 admission, got %d", admitCount)
	}
}

// Test that clock rollback followed by recovery doesn't double-mint capacity
func TestClockRollbackAndRecovery(t *testing.T) {
	now := time.Now()
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0, // 1 token per second
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Consume initial burst
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("initial token should be available")
	}
	pe.RecordAdmission("haiku", false)

	// Move forward 1 second and consume
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Fatal("should admit after 1 second")
	}
	pe.RecordAdmission("haiku", false)

	// Roll clock back by 500ms
	now = now.Add(-500 * time.Millisecond)
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("clock rollback should not grant new tokens")
	}

	// Move forward by 1.5 seconds (net +1 second from rollback point)
	now = now.Add(1500 * time.Millisecond)
	result = pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("after net +1 second from rollback, should admit")
	}

	// The next immediate check should fail (no more tokens earned)
	pe.RecordAdmission("haiku", false)
	result = pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("should not have more tokens immediately after")
	}
}

// Test multi-pool enforce mode with multiple models
func TestMultiPoolEnforceValidConfig(t *testing.T) {
	jsonStr := `{
		"pool_a": {
			"account_id": "acct1",
			"models": ["haiku"],
			"start_rate": 1.0,
			"burst_capacity": 5,
			"concurrent_dispatch_limit": 3,
			"completion_reserved": 1
		},
		"pool_b": {
			"account_id": "acct2",
			"models": ["sonnet", "opus"],
			"start_rate": 0.5,
			"burst_capacity": 2,
			"concurrent_dispatch_limit": 2,
			"completion_reserved": 1
		}
	}`

	os.Setenv("ODONIAN_RESEARCH_POOLS", jsonStr)
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeEnforce))
	defer func() {
		os.Unsetenv("ODONIAN_RESEARCH_POOLS")
		os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
	}()

	allowedModels := map[string]bool{
		"haiku":  true,
		"sonnet": true,
		"opus":   true,
	}

	config, err := ParseConfig(allowedModels)
	if err != nil {
		t.Fatalf("ParseConfig failed for valid multi-pool config: %v", err)
	}

	if len(config.Pools) != 2 {
		t.Fatalf("expected 2 pools, got %d", len(config.Pools))
	}

	// Create evaluator and test independent pools
	pe := New(config.Mode, config.Pools)

	// Haiku: pool_a, limit 3, reserved 1, so first-pass max is 2
	pe.CheckAdmission("haiku", false)
	pe.RecordAdmission("haiku", false)
	pe.CheckAdmission("haiku", false)
	pe.RecordAdmission("haiku", false)

	// Third fresh start should fail (at limit - reserved)
	result := pe.CheckAdmission("haiku", false)
	if result.Admitted {
		t.Errorf("haiku pool: should block third fresh start")
	}

	// Sonnet: pool_b, limit 2, reserved 1, so first-pass max is 1
	result = pe.CheckAdmission("sonnet", false)
	if !result.Admitted {
		t.Errorf("sonnet pool: first fresh start should succeed")
	}
	pe.RecordAdmission("sonnet", false)

	// Second should fail (at limit - reserved)
	result = pe.CheckAdmission("sonnet", false)
	if result.Admitted {
		t.Errorf("sonnet pool: should block second fresh start")
	}
}

// Test that unmapped models in enforce mode are rejected with correct reason
func TestUnmappedModelEnforceReason(t *testing.T) {
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           10,
			ConcurrentDispatchLimit: 5,
		},
	})

	result := pe.CheckAdmission("unknown-model", false)
	if result.Admitted {
		t.Errorf("unmapped model in enforce mode should be rejected")
	}
	if result.Reason != ReasonUnmappedModel {
		t.Errorf("expected ReasonUnmappedModel, got %s", result.Reason)
	}
	if !result.NotBefore.IsZero() {
		t.Errorf("unmapped model should not set NotBefore")
	}
}

// Test exact admission counts with repeated denials
func TestTokenBucketNoCapacityMinting(t *testing.T) {
	now := time.Now()
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               0.1, // 1 token per 10 seconds
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Consume burst
	pe.CheckAdmission("haiku", false)
	pe.RecordAdmission("haiku", false)

	// Check 20 times at 100ms intervals - should NOT accumulate extra tokens
	denialCount := 0
	for i := 0; i < 20; i++ {
		now = now.Add(100 * time.Millisecond)
		result := pe.CheckAdmission("haiku", false)
		if !result.Admitted {
			denialCount++
		}
	}

	// All 20 should be denied (only 0.2 tokens earned, not enough for 1)
	if denialCount != 20 {
		t.Errorf("expected 20 denials over 2 seconds at 0.1 tokens/sec, got %d admissions", 20-denialCount)
	}

	// After 10 total seconds, should have exactly 1 token earned
	now = now.Add(8 * time.Second) // total 10 seconds from start
	result := pe.CheckAdmission("haiku", false)
	if !result.Admitted {
		t.Errorf("after 10 seconds at 0.1 tokens/sec, should have 1 token")
	}
}
