package tuiconfig

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml"
)

// Config represents the resolved TUI configuration.
type Config struct {
	URL            string
	Token          string
	Actor          string
	DefaultProject string
	PollInterval   time.Duration

	// HiddenStates carries the hidden_states key from the config file as
	// plain strings; names are not validated here. HiddenStatesSet tells the
	// three cases apart: false means the key is absent (or invalid) and the
	// caller uses its built-in default; true with an empty slice means show
	// every state; true with names means hide exactly those.
	HiddenStates    []string
	HiddenStatesSet bool
}

// LoadConfig resolves configuration from file, environment, and flags.
// Precedence: flags > env > file > defaults.
func LoadConfig(flagURL, flagToken, flagActor string) (*Config, error) {
	cfg := &Config{
		PollInterval: 2 * time.Second,
	}

	// Load from config file first
	if err := loadFromFile(cfg); err != nil {
		return nil, err
	}

	// Override with environment variables
	if envURL := os.Getenv("ODONIAN_URL"); envURL != "" {
		cfg.URL = envURL
	}
	if envToken := os.Getenv("ODONIAN_TOKEN"); envToken != "" {
		cfg.Token = envToken
	}
	if envActor := os.Getenv("ODONIAN_ACTOR"); envActor != "" {
		cfg.Actor = envActor
	}

	// Override with flags (highest precedence)
	if flagURL != "" {
		cfg.URL = flagURL
	}
	if flagToken != "" {
		cfg.Token = flagToken
	}
	if flagActor != "" {
		cfg.Actor = flagActor
	}

	// Default actor to $USER if not set
	if cfg.Actor == "" {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("failed to get current user: %w", err)
		}
		cfg.Actor = u.Username
	}

	// Validate required fields
	if cfg.URL == "" {
		return nil, fmt.Errorf("missing ODONIAN_URL (set via config file, ODONIAN_URL env, or --url flag)")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("missing ODONIAN_TOKEN (set via config file, ODONIAN_TOKEN env, or --token flag)")
	}

	return cfg, nil
}

// fileConfig is the structure for parsing the TOML config file.
type fileConfig struct {
	URL            string      `toml:"url"`
	Token          string      `toml:"token"`
	Actor          string      `toml:"actor"`
	DefaultProject string      `toml:"default_project"`
	PollInterval   string      `toml:"poll_interval"` // Parse as string, then duration
	HiddenStates   interface{} `toml:"hidden_states"` // checked after load: must be a list of strings
}

// loadFromFile loads configuration from ~/.config/odonian/config.toml
func loadFromFile(cfg *Config) error {
	// Resolve config file path
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// If we can't get home dir, just skip file loading
			return nil
		}
		configHome = filepath.Join(home, ".config")
	}

	configPath := filepath.Join(configHome, "odonian", "config.toml")

	// Check if file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// File doesn't exist, that's okay
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to stat config file: %w", err)
	}

	// Read and parse TOML
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	var fileCfg fileConfig
	if err := toml.Unmarshal(data, &fileCfg); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	// Check if token comes from a world-readable file and warn
	if fileCfg.Token != "" {
		if err := checkTokenFilePermissions(configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
	}

	// Apply loaded values (these have lower precedence than env/flags, so we'll override later)
	if fileCfg.URL != "" {
		cfg.URL = fileCfg.URL
	}
	if fileCfg.Token != "" {
		cfg.Token = fileCfg.Token
	}
	if fileCfg.Actor != "" {
		cfg.Actor = fileCfg.Actor
	}
	if fileCfg.DefaultProject != "" {
		cfg.DefaultProject = fileCfg.DefaultProject
	}
	if fileCfg.PollInterval != "" {
		d, err := time.ParseDuration(fileCfg.PollInterval)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to parse poll_interval %q: %v; using default 2s\n", fileCfg.PollInterval, err)
		} else {
			cfg.PollInterval = d
		}
	}

	if fileCfg.HiddenStates != nil {
		if names, ok := parseStringList(fileCfg.HiddenStates); ok {
			cfg.HiddenStates = names
			cfg.HiddenStatesSet = true
		} else {
			fmt.Fprintf(os.Stderr, "Warning: hidden_states must be a list of state names, got %v; using default\n", fileCfg.HiddenStates)
		}
	}

	return nil
}

// parseStringList converts a decoded TOML array into a string slice. It
// reports false when v is not an array or holds a non-string element.
func parseStringList(v interface{}) ([]string, bool) {
	items, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// checkTokenFilePermissions warns if the config file is world-readable or group-readable.
func checkTokenFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	mode := info.Mode()
	// Check if others or group can read the file (mode & 0o077)
	if mode&0o077 != 0 {
		return fmt.Errorf("token found in world-readable or group-readable file %s (mode %o) — consider 'chmod 600 %s'", path, mode.Perm(), path)
	}

	return nil
}
