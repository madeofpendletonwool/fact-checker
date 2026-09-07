package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.AIBaseURL != "https://api.openai.com/v1" {
		t.Errorf("AIBaseURL = %q, want default endpoint", cfg.AIBaseURL)
	}
	if cfg.AITimeout != 2*time.Minute {
		t.Errorf("AITimeout = %v, want 2m", cfg.AITimeout)
	}
	if cfg.RequestMinInterval != 500*time.Millisecond {
		t.Errorf("RequestMinInterval = %v, want 500ms", cfg.RequestMinInterval)
	}
	for _, stage := range Stages() {
		if _, ok := cfg.Models[stage]; !ok {
			t.Errorf("Models missing stage %q", stage)
		}
		price, ok := cfg.Prices[stage]
		if !ok {
			t.Errorf("Prices missing stage %q", stage)
		} else if price.Input != 0 || price.Output != 0 {
			t.Errorf("Prices[%q] = %+v, want zero default", stage, price)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("FACTCHECK_DATABASE_URL", "postgres://u:p@h:5432/db?sslmode=disable")
	t.Setenv("FACTCHECK_LISTEN_ADDR", ":9999")
	t.Setenv("FACTCHECK_AI_TIMEOUT", "90s")
	t.Setenv("FACTCHECK_MODEL_EXTRACT", "model-a")
	t.Setenv("FACTCHECK_MODEL_FACTCHECK", "model-b")
	t.Setenv("FACTCHECK_PRICE_MTOK_EXTRACT_INPUT", "1.25")
	t.Setenv("FACTCHECK_PRICE_MTOK_EXTRACT_OUTPUT", "10")
	t.Setenv("FACTCHECK_BUDGET_PER_RUN_USD", "5.50")
	t.Setenv("FACTCHECK_REQUEST_MIN_INTERVAL", "1s")
	t.Setenv("FACTCHECK_REQUEST_INTERVAL_EXTRACT", "2s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DatabaseURL != "postgres://u:p@h:5432/db?sslmode=disable" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.AITimeout != 90*time.Second {
		t.Errorf("AITimeout = %v, want 90s", cfg.AITimeout)
	}
	if cfg.Models[StageExtract] != "model-a" || cfg.Models[StageFactCheck] != "model-b" {
		t.Errorf("Models = %v", cfg.Models)
	}
	if got := cfg.Prices[StageExtract]; got.Input != 1.25 || got.Output != 10 {
		t.Errorf("Prices[extract] = %+v, want 1.25/10", got)
	}
	if cfg.BudgetPerRunUSD != 5.50 {
		t.Errorf("BudgetPerRunUSD = %v, want 5.50", cfg.BudgetPerRunUSD)
	}
	if cfg.RequestMinInterval != time.Second {
		t.Errorf("RequestMinInterval = %v, want 1s", cfg.RequestMinInterval)
	}
	if cfg.StageIntervals[StageExtract] != 2*time.Second {
		t.Errorf("StageIntervals[extract] = %v, want 2s", cfg.StageIntervals[StageExtract])
	}
	if _, ok := cfg.StageIntervals[StageValidate]; ok {
		t.Errorf("StageIntervals[validate] set, want unset (global interval applies)")
	}
}

func TestLoadMalformedValues(t *testing.T) {
	cases := map[string]string{
		"FACTCHECK_AI_TIMEOUT":              "not-a-duration",
		"FACTCHECK_BUDGET_TOTAL_USD":        "free",
		"FACTCHECK_PRICE_MTOK_REVIEW_INPUT": "0.0.1",
	}
	for key, value := range cases {
		t.Setenv(key, value)
		cfg, err := Load()
		if err == nil {
			t.Errorf("Load with %s=%q: expected error, got %+v", key, value, cfg)
		}
	}
}

func TestValidateDatabase(t *testing.T) {
	cfg := &Config{}
	if err := cfg.ValidateDatabase(); err == nil {
		t.Error("ValidateDatabase on empty URL: expected error")
	}
	cfg.DatabaseURL = "postgres://x"
	if err := cfg.ValidateDatabase(); err != nil {
		t.Errorf("ValidateDatabase on set URL: %v", err)
	}
}
