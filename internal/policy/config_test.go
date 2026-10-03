package policy

import (
	"strings"
	"testing"
)

var allowed = []string{"haiku", "sonnet", "opus"}

const goodPool = `{"muse":{"account_id":"meta","models":["haiku"],"start_rate":0.5,"burst_capacity":5,"concurrent_dispatch_limit":3,"completion_reserved":1},
"claude":{"account_id":"anthropic","models":["sonnet","opus"],"start_rate":2,"burst_capacity":20,"concurrent_dispatch_limit":10,"completion_reserved":5}}`

func TestParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		pools   string
		allowed []string
		wantErr string // substring; empty means success
	}{
		{"empty mode is disabled", "", "", allowed, ""},
		{"disabled ignores pools", "disabled", "not json", allowed, ""},
		{"disabled with whitespace mode", "  ", "", allowed, ""},
		{"observe without pools", "observe", "", allowed, ""},
		{"observe partial mapping", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, ""},
		{"enforce full multi-pool mapping", "enforce", goodPool, allowed, ""},
		{"unknown mode", "yes", "", allowed, "invalid ODONIAN_RESEARCH_POLICY_MODE"},
		{"enforce without pools", "enforce", "", allowed, "unmapped: haiku, opus, sonnet"},
		{"enforce unmapped model", "enforce", `{"a":{"account_id":"x","models":["opus","sonnet"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "unmapped: haiku"},
		{"enforce with no allowed models", "enforce", "", nil, ""},
		{"malformed json", "observe", `{`, allowed, "invalid ODONIAN_RESEARCH_POOLS"},
		{"trailing data", "observe", goodPool + ` {}`, allowed, "trailing"},
		{"unknown field", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0,"extra":1}}`, allowed, "unknown field"},
		{"non-numeric rate", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":"fast","burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "invalid ODONIAN_RESEARCH_POOLS"},
		{"fractional burst", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1.5,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "invalid ODONIAN_RESEARCH_POOLS"},
		{"overflowing rate is not nonfinite-accepted", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1e999,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "invalid ODONIAN_RESEARCH_POOLS"},
		{"missing account", "observe", `{"a":{"models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "missing account_id"},
		{"missing models", "observe", `{"a":{"account_id":"x","start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "missing models"},
		{"missing rate", "observe", `{"a":{"account_id":"x","models":["opus"],"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "missing start_rate"},
		{"missing burst", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "missing burst_capacity"},
		{"missing limit", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"completion_reserved":0}}`, allowed, "missing concurrent_dispatch_limit"},
		{"missing reserve", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1}}`, allowed, "missing completion_reserved"},
		{"empty account", "observe", `{"a":{"account_id":"","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "account_id must not be empty"},
		{"empty models", "observe", `{"a":{"account_id":"x","models":[],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "models must not be empty"},
		{"model outside allowlist", "observe", `{"a":{"account_id":"x","models":["gpt"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, `model "gpt" is not in the allowed`},
		{"zero rate", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":0,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "start_rate must be finite and positive"},
		{"negative rate", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":-1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "start_rate must be finite and positive"},
		{"zero burst", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":0,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "burst_capacity must be at least 1"},
		{"negative burst", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":-2,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "burst_capacity must be at least 1"},
		{"zero limit", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":0,"completion_reserved":0}}`, allowed, "concurrent_dispatch_limit must be at least 1"},
		{"negative reserve", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":-1}}`, allowed, "completion_reserved must not be negative"},
		{"reserve above limit", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":2,"completion_reserved":3}}`, allowed, "exceeds concurrent_dispatch_limit"},
		{"reserve equal to limit is allowed", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":2,"completion_reserved":2}}`, allowed, ""},
		{"model mapped twice", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0},"b":{"account_id":"y","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, `model "opus" is mapped twice (pools "a" and "b")`},
		{"model repeated within a pool", "observe", `{"a":{"account_id":"x","models":["opus","opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, `mapped twice`},
		{"account split across pools", "observe", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0},"b":{"account_id":"x","models":["haiku"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, allowed, "account \"x\" is already used by pool \"a\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ParseConfig(c.mode, c.pools, c.allowed)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if _, err := New(t0, cfg); err != nil {
					t.Fatalf("parsed config rejected by New: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestParseConfigIsDeterministic(t *testing.T) {
	cfg, err := ParseConfig("enforce", goodPool, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeEnforce || len(cfg.Pools) != 2 || cfg.Pools[0].Name != "claude" || cfg.Pools[1].Name != "muse" {
		t.Fatalf("pools not sorted by name: %+v", cfg.Pools)
	}
	p := cfg.Pools[0]
	if p.AccountID != "anthropic" || p.StartRate != 2 || p.BurstCapacity != 20 || p.ConcurrentDispatchLimit != 10 || p.CompletionReserved != 5 || len(p.Models) != 2 {
		t.Fatalf("pool fields = %+v", p)
	}
	// Multiple problems: the error must be the same on every run.
	for i := 0; i < 20; i++ {
		_, err := ParseConfig("enforce", `{"a":{"account_id":"x","models":["opus"],"start_rate":1,"burst_capacity":1,"concurrent_dispatch_limit":1,"completion_reserved":0}}`, []string{"z", "y", "x", "opus"})
		if err == nil || !strings.HasSuffix(err.Error(), "unmapped: x, y, z") {
			t.Fatalf("nondeterministic or wrong error: %v", err)
		}
	}
}

func TestValidateDirectConfigs(t *testing.T) {
	nan := func() float64 { z := 0.0; return z / z }()
	inf := func() float64 { z := 0.0; return 1 / z }()
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"nan rate", cfgOf(ModeEnforce, allModelsPool(nan, 1, 1, 0)), "start_rate"},
		{"inf rate", cfgOf(ModeEnforce, allModelsPool(inf, 1, 1, 0)), "start_rate"},
		{"negative retry", func() Config { c := cfgOf(ModeEnforce, allModelsPool(1, 1, 1, 0)); c.ConcurrencyRetry = -1; return c }(), "retry"},
		{"empty pool name", cfgOf(ModeEnforce, pool("", "a", 1, 1, 1, 0, "haiku", "sonnet", "opus")), "pool name"},
		{"duplicate pool name", cfgOf(ModeEnforce, pool("p", "a", 1, 1, 1, 0, "haiku"), pool("p", "b", 1, 1, 1, 0, "sonnet", "opus")), "duplicate pool name"},
		{"disabled needs nothing", Config{Mode: ModeDisabled}, ""},
		{"disabled ignores bad pools", cfgOf(ModeDisabled, allModelsPool(-1, 0, 0, 9)), ""},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		if c.want == "" && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: error = %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	if _, err := New(t0, cfgOf(ModeEnforce)); err == nil {
		t.Fatal("enforce with no pools and allowed models must be rejected")
	}
}
