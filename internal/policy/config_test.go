package policy

import (
	"os"
	"testing"
)

func TestParseValidPoolConfig(t *testing.T) {
	jsonStr := `{
		"default": {
			"account_id": "acct1",
			"models": ["haiku", "sonnet"],
			"start_rate": 1.5,
			"burst_capacity": 10,
			"concurrent_dispatch_limit": 5,
			"completion_reserved": 2
		}
	}`

	allowedModels := map[string]bool{
		"haiku":  true,
		"sonnet": true,
	}

	os.Setenv("ODONIAN_RESEARCH_POOLS", jsonStr)
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeEnforce))
	defer func() {
		os.Unsetenv("ODONIAN_RESEARCH_POOLS")
		os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
	}()

	config, err := ParseConfig(allowedModels)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if config.Mode != ModeEnforce {
		t.Errorf("expected enforce mode, got %s", config.Mode)
	}

	if len(config.Pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(config.Pools))
	}

	pool := config.Pools[0]
	if pool.Name != "default" {
		t.Errorf("expected pool name 'default', got %s", pool.Name)
	}
	if pool.AccountID != "acct1" {
		t.Errorf("expected account acct1, got %s", pool.AccountID)
	}
	if pool.StartRate != 1.5 {
		t.Errorf("expected start_rate 1.5, got %v", pool.StartRate)
	}
	if pool.BurstCapacity != 10 {
		t.Errorf("expected burst_capacity 10, got %d", pool.BurstCapacity)
	}
	if pool.ConcurrentDispatchLimit != 5 {
		t.Errorf("expected concurrent limit 5, got %d", pool.ConcurrentDispatchLimit)
	}
	if pool.CompletionReserved != 2 {
		t.Errorf("expected completion reserved 2, got %d", pool.CompletionReserved)
	}
	if !pool.Models["haiku"] || !pool.Models["sonnet"] {
		t.Errorf("expected haiku and sonnet in models")
	}
}

func TestParseDisabledMode(t *testing.T) {
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeDisabled))
	defer os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")

	config, err := ParseConfig(map[string]bool{"haiku": true})
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if config.Mode != ModeDisabled {
		t.Errorf("expected disabled mode, got %s", config.Mode)
	}
	if len(config.Pools) != 0 {
		t.Errorf("disabled mode should have no pools")
	}
}

func TestParseObserveModeNoPoolsValid(t *testing.T) {
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeObserve))
	defer os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")

	config, err := ParseConfig(map[string]bool{"haiku": true})
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if config.Mode != ModeObserve {
		t.Errorf("expected observe mode")
	}
}

func TestParseEnforceModeNeedsConfiguration(t *testing.T) {
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeEnforce))
	defer os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")

	_, err := ParseConfig(map[string]bool{"haiku": true})
	if err == nil {
		t.Errorf("enforce mode without pools should error")
	}
}

func TestParseInvalidMode(t *testing.T) {
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", "invalid")
	defer os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")

	_, err := ParseConfig(map[string]bool{"haiku": true})
	if err == nil {
		t.Errorf("invalid mode should error")
	}
}

func TestParseInvalidJSON(t *testing.T) {
	os.Setenv("ODONIAN_RESEARCH_POOLS", "not valid json")
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeEnforce))
	defer func() {
		os.Unsetenv("ODONIAN_RESEARCH_POOLS")
		os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
	}()

	_, err := ParseConfig(map[string]bool{"haiku": true})
	if err == nil {
		t.Errorf("invalid JSON should error")
	}
}

func TestParseMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		jsonCfg string
		field   string
	}{
		{
			"missing account_id",
			`{"p": {"models": ["haiku"], "start_rate": 1, "burst_capacity": 1, "concurrent_dispatch_limit": 1, "completion_reserved": 0}}`,
			"account_id",
		},
		{
			"missing models",
			`{"p": {"account_id": "a1", "start_rate": 1, "burst_capacity": 1, "concurrent_dispatch_limit": 1, "completion_reserved": 0}}`,
			"models",
		},
		{
			"empty models array",
			`{"p": {"account_id": "a1", "models": [], "start_rate": 1, "burst_capacity": 1, "concurrent_dispatch_limit": 1, "completion_reserved": 0}}`,
			"models",
		},
		{
			"missing start_rate",
			`{"p": {"account_id": "a1", "models": ["haiku"], "burst_capacity": 1, "concurrent_dispatch_limit": 1, "completion_reserved": 0}}`,
			"start_rate",
		},
		{
			"missing burst_capacity",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": 1, "concurrent_dispatch_limit": 1, "completion_reserved": 0}}`,
			"burst_capacity",
		},
		{
			"missing concurrent_dispatch_limit",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": 1, "burst_capacity": 1, "completion_reserved": 0}}`,
			"concurrent_dispatch_limit",
		},
		{
			"missing completion_reserved",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": 1, "burst_capacity": 1, "concurrent_dispatch_limit": 1}}`,
			"completion_reserved",
		},
	}

	allowedModels := map[string]bool{"haiku": true}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("ODONIAN_RESEARCH_POOLS", tt.jsonCfg)
			os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeEnforce))
			defer func() {
				os.Unsetenv("ODONIAN_RESEARCH_POOLS")
				os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
			}()

			_, err := ParseConfig(allowedModels)
			if err == nil {
				t.Errorf("expected error for missing/invalid %s", tt.field)
			}
		})
	}
}

func TestParseInvalidFieldValues(t *testing.T) {
	tests := []struct {
		name    string
		jsonCfg string
		issue   string
	}{
		{
			"negative start_rate",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": -1, "burst_capacity": 1, "concurrent_dispatch_limit": 1, "completion_reserved": 0}}`,
			"negative start_rate",
		},
		{
			"zero burst_capacity",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": 1, "burst_capacity": 0, "concurrent_dispatch_limit": 1, "completion_reserved": 0}}`,
			"zero burst_capacity",
		},
		{
			"zero concurrent_dispatch_limit",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": 1, "burst_capacity": 1, "concurrent_dispatch_limit": 0, "completion_reserved": 0}}`,
			"zero concurrent limit",
		},
		{
			"negative completion_reserved",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": 1, "burst_capacity": 1, "concurrent_dispatch_limit": 1, "completion_reserved": -1}}`,
			"negative completion",
		},
		{
			"completion_reserved exceeds limit",
			`{"p": {"account_id": "a1", "models": ["haiku"], "start_rate": 1, "burst_capacity": 1, "concurrent_dispatch_limit": 2, "completion_reserved": 5}}`,
			"completion > limit",
		},
	}

	allowedModels := map[string]bool{"haiku": true}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("ODONIAN_RESEARCH_POOLS", tt.jsonCfg)
			os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeEnforce))
			defer func() {
				os.Unsetenv("ODONIAN_RESEARCH_POOLS")
				os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
			}()

			_, err := ParseConfig(allowedModels)
			if err == nil {
				t.Errorf("expected error for %s", tt.issue)
			}
		})
	}
}

func TestParseUnmappedModel(t *testing.T) {
	jsonStr := `{
		"default": {
			"account_id": "acct1",
			"models": ["haiku"],
			"start_rate": 1,
			"burst_capacity": 1,
			"concurrent_dispatch_limit": 1,
			"completion_reserved": 0
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
	}

	_, err := ParseConfig(allowedModels)
	if err == nil {
		t.Errorf("enforce mode: unmapped model sonnet should error")
	}
}

func TestParseDuplicateModelMapping(t *testing.T) {
	jsonStr := `{
		"pool1": {
			"account_id": "acct1",
			"models": ["haiku"],
			"start_rate": 1,
			"burst_capacity": 1,
			"concurrent_dispatch_limit": 1,
			"completion_reserved": 0
		},
		"pool2": {
			"account_id": "acct2",
			"models": ["haiku"],
			"start_rate": 1,
			"burst_capacity": 1,
			"concurrent_dispatch_limit": 1,
			"completion_reserved": 0
		}
	}`

	os.Setenv("ODONIAN_RESEARCH_POOLS", jsonStr)
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeObserve))
	defer func() {
		os.Unsetenv("ODONIAN_RESEARCH_POOLS")
		os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
	}()

	allowedModels := map[string]bool{"haiku": true}

	_, err := ParseConfig(allowedModels)
	if err == nil {
		t.Errorf("duplicate model in multiple pools should error")
	}
}

func TestParseDuplicateAccountID(t *testing.T) {
	jsonStr := `{
		"pool1": {
			"account_id": "acct1",
			"models": ["haiku"],
			"start_rate": 1,
			"burst_capacity": 1,
			"concurrent_dispatch_limit": 1,
			"completion_reserved": 0
		},
		"pool2": {
			"account_id": "acct1",
			"models": ["sonnet"],
			"start_rate": 1,
			"burst_capacity": 1,
			"concurrent_dispatch_limit": 1,
			"completion_reserved": 0
		}
	}`

	os.Setenv("ODONIAN_RESEARCH_POOLS", jsonStr)
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeObserve))
	defer func() {
		os.Unsetenv("ODONIAN_RESEARCH_POOLS")
		os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
	}()

	allowedModels := map[string]bool{
		"haiku":  true,
		"sonnet": true,
	}

	_, err := ParseConfig(allowedModels)
	if err == nil {
		t.Errorf("duplicate account ID should error")
	}
}

func TestParseMultipleValidPools(t *testing.T) {
	jsonStr := `{
		"muse": {
			"account_id": "acct1",
			"models": ["haiku"],
			"start_rate": 0.5,
			"burst_capacity": 5,
			"concurrent_dispatch_limit": 3,
			"completion_reserved": 1
		},
		"opus": {
			"account_id": "acct2",
			"models": ["opus"],
			"start_rate": 2.0,
			"burst_capacity": 20,
			"concurrent_dispatch_limit": 10,
			"completion_reserved": 5
		}
	}`

	os.Setenv("ODONIAN_RESEARCH_POOLS", jsonStr)
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeObserve))
	defer func() {
		os.Unsetenv("ODONIAN_RESEARCH_POOLS")
		os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
	}()

	allowedModels := map[string]bool{
		"haiku": true,
		"opus":  true,
	}

	config, err := ParseConfig(allowedModels)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if len(config.Pools) != 2 {
		t.Fatalf("expected 2 pools, got %d", len(config.Pools))
	}
}

func TestParseInvalidModelInPool(t *testing.T) {
	jsonStr := `{
		"pool1": {
			"account_id": "acct1",
			"models": ["unknown-model"],
			"start_rate": 1,
			"burst_capacity": 1,
			"concurrent_dispatch_limit": 1,
			"completion_reserved": 0
		}
	}`

	os.Setenv("ODONIAN_RESEARCH_POOLS", jsonStr)
	os.Setenv("ODONIAN_RESEARCH_POLICY_MODE", string(ModeObserve))
	defer func() {
		os.Unsetenv("ODONIAN_RESEARCH_POOLS")
		os.Unsetenv("ODONIAN_RESEARCH_POLICY_MODE")
	}()

	allowedModels := map[string]bool{"haiku": true}

	_, err := ParseConfig(allowedModels)
	if err == nil {
		t.Errorf("should error when pool references unknown model")
	}
}
