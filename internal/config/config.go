// Package config loads fact-checker runtime configuration from the
// environment.
//
// Every variable this package reads is documented in .env.example; the two
// files must stay in sync. Real values live only in a gitignored .env file —
// placeholders are the only thing committed.
//
// The AI-provider block (endpoint, key, per-stage models, prices per million
// tokens, budget caps, request intervals) is parsed here from day one so the
// budgeted, rate-limited pipeline stages have a stable configuration contract
// to build on, even though no stage makes model calls yet.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Stage names a pipeline stage that makes (or supervises) model calls.
type Stage string

// Pipeline stages, in pipeline order.
const (
	StageExtract   Stage = "extract"
	StageValidate  Stage = "validate"
	StageFactCheck Stage = "factcheck"
	StageReview    Stage = "review"
)

// Stages returns the stages in pipeline order.
func Stages() []Stage {
	return []Stage{StageExtract, StageValidate, StageFactCheck, StageReview}
}

// Price is what a stage's model costs, in USD per one million tokens.
type Price struct {
	Input  float64
	Output float64
}

// Config is the full runtime configuration for the fact-checker binary.
type Config struct {
	// DatabaseURL is the Postgres connection string.
	DatabaseURL string
	// ListenAddr is the HTTP listen address for the operational surface.
	ListenAddr string
	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// AIBaseURL is an OpenAI-compatible endpoint.
	AIBaseURL string
	// AIKey authenticates against AIBaseURL. Never logged.
	AIKey string
	// AITimeout bounds a single model call.
	AITimeout time.Duration

	// Models maps each stage to its model name.
	Models map[Stage]string
	// Prices maps each stage to its USD-per-MTok pricing.
	Prices map[Stage]Price

	// BudgetTotalUSD caps lifetime spend; BudgetPerRunUSD caps one run.
	// Zero means unset (no cap enforced yet); stages interpret the caps.
	BudgetTotalUSD  float64
	BudgetPerRunUSD float64

	// RequestMinInterval is the global minimum interval between model
	// requests. StageIntervals optionally overrides it per stage.
	RequestMinInterval time.Duration
	StageIntervals     map[Stage]time.Duration
}

// Load reads configuration from the environment, applying defaults for
// anything unset. Malformed values are errors, not silent defaults.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL: os.Getenv("FACTCHECK_DATABASE_URL"),
		ListenAddr:  getenv("FACTCHECK_LISTEN_ADDR", ":8080"),
		LogLevel:    getenv("FACTCHECK_LOG_LEVEL", "info"),

		AIBaseURL: getenv("FACTCHECK_AI_BASE_URL", "https://api.openai.com/v1"),
		AIKey:     os.Getenv("FACTCHECK_AI_API_KEY"),

		Models:         make(map[Stage]string, 4),
		Prices:         make(map[Stage]Price, 4),
		StageIntervals: make(map[Stage]time.Duration, 4),
	}

	var err error
	if cfg.AITimeout, err = getenvDuration("FACTCHECK_AI_TIMEOUT", 2*time.Minute); err != nil {
		return nil, err
	}
	if cfg.BudgetTotalUSD, err = getenvFloat("FACTCHECK_BUDGET_TOTAL_USD", 0); err != nil {
		return nil, err
	}
	if cfg.BudgetPerRunUSD, err = getenvFloat("FACTCHECK_BUDGET_PER_RUN_USD", 0); err != nil {
		return nil, err
	}
	if cfg.RequestMinInterval, err = getenvDuration("FACTCHECK_REQUEST_MIN_INTERVAL", 500*time.Millisecond); err != nil {
		return nil, err
	}

	for _, stage := range Stages() {
		name := string(stage)
		cfg.Models[stage] = os.Getenv("FACTCHECK_MODEL_" + upper(name))

		var price Price
		if price.Input, err = getenvFloat("FACTCHECK_PRICE_MTOK_"+upper(name)+"_INPUT", 0); err != nil {
			return nil, err
		}
		if price.Output, err = getenvFloat("FACTCHECK_PRICE_MTOK_"+upper(name)+"_OUTPUT", 0); err != nil {
			return nil, err
		}
		cfg.Prices[stage] = price

		if raw := os.Getenv("FACTCHECK_REQUEST_INTERVAL_" + upper(name)); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return nil, fmt.Errorf("FACTCHECK_REQUEST_INTERVAL_%s: %w", upper(name), err)
			}
			cfg.StageIntervals[stage] = d
		}
	}

	return cfg, nil
}

// ValidateDatabase returns an error if the datastore is not configured. Serve
// and migrate both require it; informational commands do not.
func (c *Config) ValidateDatabase() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("FACTCHECK_DATABASE_URL is required (see .env.example)")
	}
	return nil
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvDuration(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func getenvFloat(key string, def float64) (float64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
