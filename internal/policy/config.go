package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// DefaultConcurrencyRetry is the retry hint returned for concurrency-dependent
// outcomes when Config.ConcurrencyRetry is zero.
const DefaultConcurrencyRetry = time.Second

// Pool is one account/quota pool. All models mapped to it share one allowance,
// whichever project or worker asks. Pool state is keyed by AccountID.
type Pool struct {
	Name                    string
	AccountID               string
	Models                  []string
	StartRate               float64 // sustained starts per second; a pacing proxy, not billing
	BurstCapacity           int     // starts available at once when the pool is idle
	ConcurrentDispatchLimit int     // total active dispatches, completion work included
	CompletionReserved      int     // slots only completion work may use; 0..ConcurrentDispatchLimit
}

// Config is the full research pacing policy configuration.
type Config struct {
	Mode Mode
	// AllowedModels is the server's ODONIAN_MODELS allowlist. Pool models must
	// belong to it, and enforcement requires every entry to be mapped.
	AllowedModels []string
	Pools         []Pool
	// ConcurrencyRetry is the retry hint for concurrency-dependent outcomes;
	// zero means DefaultConcurrencyRetry.
	ConcurrencyRetry time.Duration
}

func (c Config) retryAfter() time.Duration {
	if c.ConcurrencyRetry > 0 {
		return c.ConcurrencyRetry
	}
	return DefaultConcurrencyRetry
}

func (c Config) clone() Config {
	out := c
	out.AllowedModels = append([]string(nil), c.AllowedModels...)
	out.Pools = make([]Pool, len(c.Pools))
	for i, p := range c.Pools {
		p.Models = append([]string(nil), p.Models...)
		out.Pools[i] = p
	}
	return out
}

type rawPool struct {
	AccountID               *string   `json:"account_id"`
	Models                  *[]string `json:"models"`
	StartRate               *float64  `json:"start_rate"`
	BurstCapacity           *int      `json:"burst_capacity"`
	ConcurrentDispatchLimit *int      `json:"concurrent_dispatch_limit"`
	CompletionReserved      *int      `json:"completion_reserved"`
}

// ParseConfig builds and validates a Config from the raw values of
// ODONIAN_RESEARCH_POLICY_MODE and ODONIAN_RESEARCH_POOLS. An empty mode means
// disabled, and in disabled mode the pools value is ignored. It reads no
// environment and starts nothing.
func ParseConfig(modeStr, poolsJSON string, allowedModels []string) (Config, error) {
	mode := Mode(strings.TrimSpace(modeStr))
	if mode == "" {
		mode = ModeDisabled
	}
	cfg := Config{Mode: mode, AllowedModels: append([]string(nil), allowedModels...)}
	if !mode.valid() {
		return Config{}, fmt.Errorf("invalid ODONIAN_RESEARCH_POLICY_MODE %q (want disabled, observe or enforce)", modeStr)
	}
	if mode == ModeDisabled {
		return cfg, nil
	}

	if strings.TrimSpace(poolsJSON) != "" {
		var raw map[string]rawPool
		dec := json.NewDecoder(bytes.NewReader([]byte(poolsJSON)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&raw); err != nil {
			return Config{}, fmt.Errorf("invalid ODONIAN_RESEARCH_POOLS: %w", err)
		}
		if dec.More() {
			return Config{}, fmt.Errorf("invalid ODONIAN_RESEARCH_POOLS: trailing data after JSON object")
		}
		names := make([]string, 0, len(raw))
		for name := range raw {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			p, err := raw[name].toPool(name)
			if err != nil {
				return Config{}, fmt.Errorf("ODONIAN_RESEARCH_POOLS pool %q: %w", name, err)
			}
			cfg.Pools = append(cfg.Pools, p)
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (r rawPool) toPool(name string) (Pool, error) {
	switch {
	case r.AccountID == nil:
		return Pool{}, fmt.Errorf("missing account_id")
	case r.Models == nil:
		return Pool{}, fmt.Errorf("missing models")
	case r.StartRate == nil:
		return Pool{}, fmt.Errorf("missing start_rate")
	case r.BurstCapacity == nil:
		return Pool{}, fmt.Errorf("missing burst_capacity")
	case r.ConcurrentDispatchLimit == nil:
		return Pool{}, fmt.Errorf("missing concurrent_dispatch_limit")
	case r.CompletionReserved == nil:
		return Pool{}, fmt.Errorf("missing completion_reserved")
	}
	return Pool{
		Name:                    name,
		AccountID:               *r.AccountID,
		Models:                  append([]string(nil), *r.Models...),
		StartRate:               *r.StartRate,
		BurstCapacity:           *r.BurstCapacity,
		ConcurrentDispatchLimit: *r.ConcurrentDispatchLimit,
		CompletionReserved:      *r.CompletionReserved,
	}, nil
}

// Validate rejects any configuration the evaluator must not run: unknown mode,
// invalid, nonfinite or negative numbers, impossible reservations, duplicate
// pool names/accounts/model mappings, models outside AllowedModels, and, under
// enforcement, allowed models with no pool. Errors are deterministic.
func (c Config) Validate() error {
	if !c.Mode.valid() {
		return fmt.Errorf("invalid mode %q", c.Mode)
	}
	if c.ConcurrencyRetry < 0 {
		return fmt.Errorf("concurrency retry must not be negative")
	}
	if c.Mode == ModeDisabled {
		return nil
	}

	allowed := make(map[string]bool, len(c.AllowedModels))
	for _, m := range c.AllowedModels {
		allowed[m] = true
	}
	names := make(map[string]bool)
	accounts := make(map[string]string)
	mapped := make(map[string]string)
	for _, p := range c.Pools {
		if p.Name == "" {
			return fmt.Errorf("pool name must not be empty")
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		names[p.Name] = true
		if p.AccountID == "" {
			return fmt.Errorf("pool %q: account_id must not be empty", p.Name)
		}
		if other, ok := accounts[p.AccountID]; ok {
			return fmt.Errorf("pool %q: account %q is already used by pool %q; aliases sharing an account belong in one pool", p.Name, p.AccountID, other)
		}
		accounts[p.AccountID] = p.Name
		if len(p.Models) == 0 {
			return fmt.Errorf("pool %q: models must not be empty", p.Name)
		}
		for _, m := range p.Models {
			if !allowed[m] {
				return fmt.Errorf("pool %q: model %q is not in the allowed model list", p.Name, m)
			}
			if other, ok := mapped[m]; ok {
				return fmt.Errorf("model %q is mapped twice (pools %q and %q)", m, other, p.Name)
			}
			mapped[m] = p.Name
		}
		if math.IsNaN(p.StartRate) || math.IsInf(p.StartRate, 0) || p.StartRate <= 0 {
			return fmt.Errorf("pool %q: start_rate must be finite and positive", p.Name)
		}
		if p.BurstCapacity < 1 {
			return fmt.Errorf("pool %q: burst_capacity must be at least 1", p.Name)
		}
		if p.ConcurrentDispatchLimit < 1 {
			return fmt.Errorf("pool %q: concurrent_dispatch_limit must be at least 1", p.Name)
		}
		if p.CompletionReserved < 0 {
			return fmt.Errorf("pool %q: completion_reserved must not be negative", p.Name)
		}
		if p.CompletionReserved > p.ConcurrentDispatchLimit {
			return fmt.Errorf("pool %q: completion_reserved (%d) exceeds concurrent_dispatch_limit (%d)", p.Name, p.CompletionReserved, p.ConcurrentDispatchLimit)
		}
	}

	if c.Mode == ModeEnforce {
		var unmapped []string
		for m := range allowed {
			if _, ok := mapped[m]; !ok {
				unmapped = append(unmapped, m)
			}
		}
		if len(unmapped) > 0 {
			sort.Strings(unmapped)
			return fmt.Errorf("enforce mode requires every allowed model to be in a pool; unmapped: %s", strings.Join(unmapped, ", "))
		}
	}
	return nil
}
