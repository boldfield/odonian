package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Config holds the research pacing policy configuration.
type Config struct {
	Mode  Mode
	Pools []*Pool
}

// ParseConfig parses the research pacing policy configuration from environment variables.
// Returns a Config if successful, or an error if validation fails.
func ParseConfig(allowedModels map[string]bool) (*Config, error) {
	modeStr := os.Getenv("ODONIAN_RESEARCH_POLICY_MODE")
	if modeStr == "" {
		modeStr = string(ModeDisabled)
	}

	mode := Mode(modeStr)
	switch mode {
	case ModeDisabled, ModeObserve, ModeEnforce:
		// valid
	default:
		return nil, fmt.Errorf("invalid ODONIAN_RESEARCH_POLICY_MODE: %s", modeStr)
	}

	// If mode is disabled, return with empty pools
	if mode == ModeDisabled {
		return &Config{Mode: ModeDisabled, Pools: []*Pool{}}, nil
	}

	// Parse pools configuration from JSON
	poolsJSON := os.Getenv("ODONIAN_RESEARCH_POOLS")
	if poolsJSON == "" {
		if mode == ModeEnforce {
			return nil, fmt.Errorf("ODONIAN_RESEARCH_POLICY_MODE is enforce but ODONIAN_RESEARCH_POOLS is not set")
		}
		// observe mode with no pools is valid (all traffic is admitted)
		return &Config{Mode: mode, Pools: []*Pool{}}, nil
	}

	var poolsData map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(poolsJSON), &poolsData); err != nil {
		return nil, fmt.Errorf("invalid JSON in ODONIAN_RESEARCH_POOLS: %w", err)
	}

	// Sort pool names for deterministic iteration
	poolNames := make([]string, 0, len(poolsData))
	for name := range poolsData {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)

	pools := make([]*Pool, 0, len(poolsData))
	seenAccounts := make(map[string]bool)    // track duplicates
	allPoolModels := make(map[string]string) // model -> pool name, for duplicate detection

	for _, poolName := range poolNames {
		poolConfig := poolsData[poolName]
		pool, err := parsePoolConfig(poolName, poolConfig, allowedModels)
		if err != nil {
			return nil, fmt.Errorf("pool %s: %w", poolName, err)
		}

		// Check for duplicate model->pool mappings
		for model := range pool.Models {
			if existingPool, ok := allPoolModels[model]; ok {
				if existingPool != poolName {
					return nil, fmt.Errorf("model %s is configured in multiple pools: %s and %s", model, existingPool, poolName)
				}
			}
			allPoolModels[model] = poolName
		}

		if seenAccounts[pool.AccountID] {
			return nil, fmt.Errorf("pool %s: duplicate account %s", poolName, pool.AccountID)
		}
		seenAccounts[pool.AccountID] = true

		pools = append(pools, pool)
	}

	// If enforce mode and there are unmapped models, error
	// Compute unmapped models once, after all pools have been processed
	if mode == ModeEnforce {
		unmappedModels := make(map[string]bool)
		for model := range allowedModels {
			if _, ok := allPoolModels[model]; !ok {
				unmappedModels[model] = true
			}
		}
		if len(unmappedModels) > 0 {
			var unmapped []string
			for model := range unmappedModels {
				unmapped = append(unmapped, model)
			}
			sort.Strings(unmapped) // deterministic error message
			return nil, fmt.Errorf("enforce mode requires all allowed models to be in a pool; unmapped: %v", unmapped)
		}
	}

	return &Config{
		Mode:  mode,
		Pools: pools,
	}, nil
}

// parsePoolConfig parses a single pool configuration.
func parsePoolConfig(poolName string, poolData map[string]interface{}, allowedModels map[string]bool) (*Pool, error) {
	pool := &Pool{
		Name:   poolName,
		Models: make(map[string]bool),
	}

	// Parse account_id (required)
	accountID, ok := poolData["account_id"].(string)
	if !ok || accountID == "" {
		return nil, fmt.Errorf("missing or invalid account_id")
	}
	pool.AccountID = accountID

	// Parse models (required, must be non-empty array of valid models)
	modelsIface, ok := poolData["models"]
	if !ok {
		return nil, fmt.Errorf("missing models")
	}
	modelsSlice, ok := modelsIface.([]interface{})
	if !ok || len(modelsSlice) == 0 {
		return nil, fmt.Errorf("models must be a non-empty array")
	}
	for _, m := range modelsSlice {
		modelStr, ok := m.(string)
		if !ok {
			return nil, fmt.Errorf("models must be strings")
		}
		if !allowedModels[modelStr] {
			return nil, fmt.Errorf("model %s is not in ODONIAN_MODELS", modelStr)
		}
		pool.Models[modelStr] = true
	}

	// Parse start_rate (required, must be positive and finite)
	startRateIface, ok := poolData["start_rate"]
	if !ok {
		return nil, fmt.Errorf("missing start_rate")
	}
	startRate, err := parseFloat(startRateIface)
	if err != nil {
		return nil, fmt.Errorf("invalid start_rate: %w", err)
	}
	if startRate <= 0 || math.IsInf(startRate, 0) || math.IsNaN(startRate) {
		return nil, fmt.Errorf("start_rate must be positive and finite")
	}
	pool.StartRate = startRate

	// Parse burst_capacity (required, must be positive integer)
	burstIface, ok := poolData["burst_capacity"]
	if !ok {
		return nil, fmt.Errorf("missing burst_capacity")
	}
	burst, err := parseInt(burstIface)
	if err != nil {
		return nil, fmt.Errorf("invalid burst_capacity: %w", err)
	}
	if burst <= 0 {
		return nil, fmt.Errorf("burst_capacity must be positive")
	}
	pool.BurstCapacity = burst

	// Parse concurrent_dispatch_limit (required, must be positive integer)
	concIface, ok := poolData["concurrent_dispatch_limit"]
	if !ok {
		return nil, fmt.Errorf("missing concurrent_dispatch_limit")
	}
	conc, err := parseInt(concIface)
	if err != nil {
		return nil, fmt.Errorf("invalid concurrent_dispatch_limit: %w", err)
	}
	if conc <= 0 {
		return nil, fmt.Errorf("concurrent_dispatch_limit must be positive")
	}
	pool.ConcurrentDispatchLimit = conc

	// Parse completion_reserved (required, must be non-negative and <= total capacity)
	compIface, ok := poolData["completion_reserved"]
	if !ok {
		return nil, fmt.Errorf("missing completion_reserved")
	}
	comp, err := parseInt(compIface)
	if err != nil {
		return nil, fmt.Errorf("invalid completion_reserved: %w", err)
	}
	if comp < 0 {
		return nil, fmt.Errorf("completion_reserved must be non-negative")
	}
	if comp > pool.ConcurrentDispatchLimit {
		return nil, fmt.Errorf("completion_reserved (%d) cannot exceed concurrent_dispatch_limit (%d)", comp, pool.ConcurrentDispatchLimit)
	}
	pool.CompletionReserved = comp

	return pool, nil
}

// parseFloat parses a float from either a number or a string.
func parseFloat(val interface{}) (float64, error) {
	switch v := val.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case string:
		return strconv.ParseFloat(v, 64)
	default:
		return 0, fmt.Errorf("expected number or string, got %T", val)
	}
}

// parseInt parses an integer from either a number or a string.
func parseInt(val interface{}) (int, error) {
	switch v := val.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("expected integer, got float %v", v)
		}
		return int(v), nil
	case int:
		return v, nil
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			return 0, err
		}
		return i, nil
	default:
		return 0, fmt.Errorf("expected number or string, got %T", val)
	}
}

// ValidateConfig checks that a configuration is valid.
// Provided for external validation; ParseConfig does its own validation inline.
func ValidateConfig(config *Config, allowedModels map[string]bool) error {
	if config.Mode != ModeDisabled && config.Mode != ModeObserve && config.Mode != ModeEnforce {
		return fmt.Errorf("invalid mode: %s", config.Mode)
	}

	if config.Mode == ModeDisabled {
		return nil
	}

	if len(config.Pools) == 0 && config.Mode == ModeEnforce {
		return fmt.Errorf("enforce mode requires at least one pool")
	}

	// Check for duplicate model mappings
	seen := make(map[string]string) // model -> pool name
	for _, pool := range config.Pools {
		for model := range pool.Models {
			if existing, ok := seen[model]; ok {
				return fmt.Errorf("model %s appears in multiple pools: %s and %s", model, existing, pool.Name)
			}
			seen[model] = pool.Name
		}
	}

	// Check for unmapped models in enforce mode
	if config.Mode == ModeEnforce {
		for model := range allowedModels {
			if _, ok := seen[model]; !ok {
				return fmt.Errorf("enforce mode: model %s is not mapped to any pool", model)
			}
		}
	}

	return nil
}

// GetAllowedModelsFromEnv parses ODONIAN_MODELS into a map.
func GetAllowedModelsFromEnv() map[string]bool {
	modelsStr := os.Getenv("ODONIAN_MODELS")
	if modelsStr == "" {
		modelsStr = "haiku,sonnet,opus"
	}
	models := make(map[string]bool)
	for _, m := range strings.Split(modelsStr, ",") {
		m = strings.TrimSpace(m)
		if m != "" {
			models[m] = true
		}
	}
	return models
}
