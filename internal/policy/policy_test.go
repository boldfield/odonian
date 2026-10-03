package policy

import (
	"os"
	"testing"
	"time"
)

func TestAdmissionDisabledMode(t *testing.T) {
	pe := New(ModeDisabled, []*Pool{})
	result := pe.CheckAdmission("haiku", ResearchWrite)
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
			result := pe.CheckAdmission("unknown-model", ResearchWrite)
			if result.Admitted != tt.admitted {
				t.Errorf("mode %s: expected admitted=%v, got %v", tt.mode, tt.admitted, result.Admitted)
			}
		})
	}
}

func TestRateLimitingWithTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0, // 1 start per second
			BurstCapacity:           3,
			ConcurrentDispatchLimit: 10,
		},
	}, now)
	pe.SetClock(func() time.Time { return now })

	// Should admit the first 3 (burst capacity)
	for i := 0; i < 3; i++ {
		result := pe.CheckAdmission("haiku", ResearchWrite)
		if !result.Admitted {
			t.Fatalf("expected admission %d, got denied: %+v", i+1, result)
		}
		pe.RecordAdmission("haiku", ResearchWrite)
	}

	// Next attempt should be denied (out of tokens)
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("expected denial after burst capacity exhausted, got admitted")
	}
	if result.Reason != ReasonRateLimit {
		t.Errorf("expected rate limit reason, got %s", result.Reason)
	}

	// Move time forward by 1 second - should gain 1 token
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Errorf("after 1 second, should have 1 new token")
	}
}

func TestRateLimitClockRollback(t *testing.T) {
	now := time.Unix(1000, 0)
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	}, now)
	pe.SetClock(func() time.Time { return now })

	// Consume the burst token
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("initial token should be available")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Move forward 1 second and consume it
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("should admit after refill")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Roll clock backward - should NOT refill
	now = now.Add(-500 * time.Millisecond)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("clock rollback should not grant new tokens")
	}

	// Move forward again - should refill normally from the rollback point
	now = now.Add(2 * time.Second)
	result = pe.CheckAdmission("haiku", ResearchWrite)
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
		result := pe.CheckAdmission("haiku", ResearchWrite)
		if !result.Admitted {
			t.Fatalf("admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", ResearchWrite)
	}

	// Next should be denied
	result := pe.CheckAdmission("haiku", ResearchWrite)
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
	pe.ReleaseAdmission("haiku", ResearchWrite)
	result = pe.CheckAdmission("haiku", ResearchWrite)
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
		result := pe.CheckAdmission("haiku", ResearchWrite)
		if !result.Admitted {
			t.Fatalf("fresh admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", ResearchWrite)
	}

	// Try to admit another fresh start - should fail (reserved capacity held)
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("should deny fresh start when reserved capacity held")
	}
	if result.Reason != ReasonConcurrency {
		t.Errorf("expected concurrency reason, got %s", result.Reason)
	}

	// Completion work can use up to total limit = 5 slots
	// With 3 fresh starts, we have 2 slots available for completion
	for i := 0; i < 2; i++ {
		result := pe.CheckAdmission("haiku", ResearchReview)
		if !result.Admitted {
			t.Fatalf("completion admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", ResearchReview)
	}

	// We're now at total capacity (3 fresh + 2 completion = 5)
	// Next completion should fail (total limit reached)
	result = pe.CheckAdmission("haiku", ResearchReview)
	if result.Admitted {
		t.Errorf("should deny completion when total capacity exhausted")
	}
	if result.Reason != ReasonConcurrency {
		t.Errorf("expected concurrency reason, got %s", result.Reason)
	}

	// Release a fresh start, now we should be able to admit completion
	pe.ReleaseAdmission("haiku", ResearchWrite)
	result = pe.CheckAdmission("haiku", ResearchReview)
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
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("haiku admission 1 failed")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("haiku admission 2 failed")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Opus: burst of 1
	result = pe.CheckAdmission("opus", ResearchWrite)
	if !result.Admitted {
		t.Fatal("opus admission 1 failed")
	}
	pe.RecordAdmission("opus", ResearchWrite)

	// Both at their limits now
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("haiku should be at concurrency limit")
	}

	result = pe.CheckAdmission("opus", ResearchWrite)
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
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("initial token unavailable")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// After 1 second, should have 0.5 tokens (not enough)
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("with 0.5 tokens, should be denied")
	}

	// After 2 more seconds (3 total), should have 1.5 tokens
	now = now.Add(2 * time.Second)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Errorf("with 1.5 tokens, should be admitted")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Token count should be capped at burst capacity
	// Current: 1.5 - 1.0 (just consumed) + some fractional from the last refill
	// Next check should have some tokens
	result = pe.CheckAdmission("haiku", ResearchWrite)
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
		result := pe.CheckAdmission("haiku", ResearchWrite)
		if !result.Admitted {
			t.Fatalf("burst %d failed", i+1)
		}
		pe.RecordAdmission("haiku", ResearchWrite)
	}

	// Wait 100 seconds - should NOT exceed burst ceiling
	now = now.Add(100 * time.Second)
	for i := 0; i < 3; i++ {
		result := pe.CheckAdmission("haiku", ResearchWrite)
		if !result.Admitted {
			t.Fatalf("after 100s wait, should have capacity, iteration %d", i+1)
		}
		pe.RecordAdmission("haiku", ResearchWrite)
	}

	// Next should be denied (at ceiling)
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("should hit burst ceiling limit")
	}
}

func TestConfigurationChangesWithoutMintingCapacity(t *testing.T) {
	now := time.Unix(1000, 0)
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           2,
			ConcurrentDispatchLimit: 10,
		},
	}, now)
	pe.SetClock(func() time.Time { return now })

	// Consume burst
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("admission 1 failed")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("admission 2 failed")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Exhausted
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Fatal("should be exhausted")
	}

	// Reconfigure with lower burst - should stay exhausted
	err := pe.Reconfigure([]*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           1, // lower burst
			ConcurrentDispatchLimit: 10,
		},
	})
	if err != nil {
		t.Fatalf("reconfigure failed: %v", err)
	}

	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("after reconfigure with lower burst, should still be exhausted")
	}

	// Reconfigure with higher burst - should NOT mint new capacity
	err = pe.Reconfigure([]*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           5, // higher burst
			ConcurrentDispatchLimit: 10,
		},
	})
	if err != nil {
		t.Fatalf("reconfigure failed: %v", err)
	}

	// Move forward by 1 second to earn exactly 1 token
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Errorf("after 1 second refill, should admit 1 token")
	}

	// Next should fail (no more tokens)
	pe.RecordAdmission("haiku", ResearchWrite)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("should not have more tokens immediately after")
	}
}

func TestFractionalRefillExactCounts(t *testing.T) {
	now := time.Unix(1000, 0)
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               0.1, // 1 token per 10 seconds
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	}, now)
	pe.SetClock(func() time.Time { return now })

	// Consume initial burst token
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("initial token should be available")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Check every 100ms for 10 seconds - should get exactly 1 more token
	admitCount := 0
	for i := 0; i < 100; i++ {
		now = now.Add(100 * time.Millisecond)
		result = pe.CheckAdmission("haiku", ResearchWrite)
		if result.Admitted {
			admitCount++
			pe.RecordAdmission("haiku", ResearchWrite)
		}
	}

	// At 0.1 tokens/second, after 10 seconds we should have earned exactly 1 token
	if admitCount != 1 {
		t.Errorf("with 0.1 tokens/sec over 10s, expected 1 admission, got %d", admitCount)
	}
}

func TestClockRollbackAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0, // 1 token per second
			BurstCapacity:           2,
			ConcurrentDispatchLimit: 10,
		},
	}, now)
	pe.SetClock(func() time.Time { return now })

	// Consume initial burst (2 tokens)
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("initial token 1 should be available")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("initial token 2 should be available")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Move forward 1 second and consume
	now = now.Add(1 * time.Second)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("should admit after 1 second")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Roll clock back by 100ms
	now = now.Add(-100 * time.Millisecond)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("clock rollback should not grant new tokens")
	}

	// Move forward by 1.1 seconds (net +1 second from start of rollback)
	now = now.Add(1100 * time.Millisecond)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Errorf("after net +1 second from start, should admit")
	}

	// The next immediate check should fail (no more tokens earned)
	pe.RecordAdmission("haiku", ResearchWrite)
	result = pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("should not have more tokens immediately after")
	}
}

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
	pe.CheckAdmission("haiku", ResearchWrite)
	pe.RecordAdmission("haiku", ResearchWrite)
	pe.CheckAdmission("haiku", ResearchWrite)
	pe.RecordAdmission("haiku", ResearchWrite)

	// Third fresh start should fail (at limit - reserved)
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("haiku pool: should block third fresh start")
	}

	// Sonnet: pool_b, limit 2, reserved 1, so first-pass max is 1
	result = pe.CheckAdmission("sonnet", ResearchWrite)
	if !result.Admitted {
		t.Errorf("sonnet pool: first fresh start should succeed")
	}
	pe.RecordAdmission("sonnet", ResearchWrite)

	// Second should fail (at limit - reserved)
	result = pe.CheckAdmission("sonnet", ResearchWrite)
	if result.Admitted {
		t.Errorf("sonnet pool: should block second fresh start")
	}
}

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

	result := pe.CheckAdmission("unknown-model", ResearchWrite)
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

func TestTokenBucketNoCapacityMinting(t *testing.T) {
	now := time.Unix(1000, 0)
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               0.1, // 1 token per 10 seconds
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 10,
		},
	}, now)
	pe.SetClock(func() time.Time { return now })

	// Consume burst
	pe.CheckAdmission("haiku", ResearchWrite)
	pe.RecordAdmission("haiku", ResearchWrite)

	// Check 20 times at 100ms intervals - should NOT accumulate extra tokens
	denialCount := 0
	for i := 0; i < 20; i++ {
		now = now.Add(100 * time.Millisecond)
		result := pe.CheckAdmission("haiku", ResearchWrite)
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
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Errorf("after 10 seconds at 0.1 tokens/sec, should have 1 token")
	}
}

// TestFirstPassWithCompletionAtCeiling tests that first-pass work respects total ceiling
// even when completion work is at the ceiling.
func TestFirstPassWithCompletionAtCeiling(t *testing.T) {
	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               100.0,
			BurstCapacity:           1000,
			ConcurrentDispatchLimit: 2,
			CompletionReserved:      1,
		},
	})

	// Admit 2 completion work (uses all capacity)
	for i := 0; i < 2; i++ {
		result := pe.CheckAdmission("haiku", ResearchReview)
		if !result.Admitted {
			t.Fatalf("completion admission %d should succeed", i+1)
		}
		pe.RecordAdmission("haiku", ResearchReview)
	}

	// Now we're at total capacity (2 completion work)
	// Try to admit first-pass work - should fail (total ceiling exceeded)
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if result.Admitted {
		t.Errorf("should deny first-pass when total ceiling reached")
	}
	if result.Reason != ReasonConcurrency {
		t.Errorf("expected concurrency reason, got %s", result.Reason)
	}
}

// TestObserveModeNeverDenies tests that observe mode always admits.
func TestObserveModeNeverDenies(t *testing.T) {
	now := time.Now()
	pe := New(ModeObserve, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               0.01, // very slow rate
			BurstCapacity:           1,
			ConcurrentDispatchLimit: 1,
		},
	})
	pe.SetClock(func() time.Time { return now })

	// Admit first one
	result := pe.CheckAdmission("haiku", ResearchWrite)
	if !result.Admitted {
		t.Fatal("first check should admit")
	}
	pe.RecordAdmission("haiku", ResearchWrite)

	// Try many more - should all be admitted in observe mode
	for i := 0; i < 10; i++ {
		result = pe.CheckAdmission("haiku", ResearchWrite)
		if !result.Admitted {
			t.Errorf("observe mode iteration %d: should admit", i+1)
		}
	}
}

// TestWorkClassesCovered tests that all work classes behave correctly.
func TestWorkClassesCovered(t *testing.T) {
	tests := []struct {
		name     string
		class    WorkClass
		admitted bool
		enforced bool
	}{
		{"ResearchWrite", ResearchWrite, true, true},
		{"ResearchReview", ResearchReview, true, true},
		{"BuildDesign", BuildDesign, true, false}, // always admitted, not paced
	}

	pe := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               1.0,
			BurstCapacity:           10,
			ConcurrentDispatchLimit: 10,
		},
	})

	for _, tt := range tests {
		result := pe.CheckAdmission("haiku", tt.class)
		if !result.Admitted {
			t.Errorf("%s: expected admitted, got denied", tt.name)
		}

		// BuildDesign work should not affect concurrency
		if tt.class != BuildDesign {
			pe.RecordAdmission("haiku", tt.class)
		}
	}

	// Check that only ResearchWrite and ResearchReview affected concurrency
	// BuildDesign did not count
	pe2 := New(ModeEnforce, []*Pool{
		{
			Name:                    "main",
			Models:                  map[string]bool{"haiku": true},
			AccountID:               "acct1",
			StartRate:               100.0,
			BurstCapacity:           1000,
			ConcurrentDispatchLimit: 2,
		},
	})

	// Admit 2 research writes
	for i := 0; i < 2; i++ {
		result := pe2.CheckAdmission("haiku", ResearchWrite)
		if !result.Admitted {
			t.Fatalf("research write admission %d should succeed", i+1)
		}
		pe2.RecordAdmission("haiku", ResearchWrite)
	}

	// BuildDesign should still be admitted even though we're at capacity
	result := pe2.CheckAdmission("haiku", BuildDesign)
	if !result.Admitted {
		t.Errorf("build/design should always be admitted, even at capacity")
	}
}
