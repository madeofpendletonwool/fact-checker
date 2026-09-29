package arda

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/extract"
	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/testdb"
	"github.com/madeofpendletonwool/fact-checker/validate"
)

// Compile-time proof the Arda-shaped adapter satisfies the validation
// contract alongside the extraction one.
var (
	_ extract.Adapter  = (*Adapter)(nil)
	_ validate.Adapter = (*Adapter)(nil)
)

// TestArdaValidationPortProof runs the generic validation engine over the
// graph the Arda-shaped adapter's extraction produced — offline, with no
// engine changes and no model calls.
func TestArdaValidationPortProof(t *testing.T) {
	url := testdb.Acquire(t, "arda_validate")
	if err := migrations.Up(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), pilotSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	raw, err := os.ReadFile("testdata/arda_pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := loaded.(*Adapter)

	if _, err := extract.Run(context.Background(), pool, adapter,
		&promptRecorder{model: "reference-model"}, extract.DefaultOptions()); err != nil {
		t.Fatal(err)
	}

	opts := validate.DefaultOptions()
	opts.Offline = true
	stats, err := validate.Run(context.Background(), pool, adapter, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Offline || stats.Claims.Total != 3 {
		t.Fatalf("offline stats wrong: %+v", stats)
	}
	// The pilot graph is internally consistent: every established claim
	// carries its Tier-1 citation, no dangling references, no duplicates,
	// and the one contested tradition was registered by extraction, so
	// the engine finds nothing to add.
	if stats.ErrorCount() != 0 || stats.Flags.Total != 0 {
		t.Fatalf("pilot graph must validate clean: %+v", stats.Flags)
	}
	if stats.Machine.ContradictionsCreated != 0 && stats.Machine.ContradictionsReused != 0 {
		t.Fatalf("unexpected contradiction churn: %+v", stats.Machine)
	}

	// The adapter's validation contract carries the domain framing.
	brief := adapter.ValidationBrief()
	if brief.Name != "The History of Arda" || adapter.ReferenceSource() != "tolkien-gateway" {
		t.Fatalf("validation brief wrong: %+v (source %s)", brief, adapter.ReferenceSource())
	}
}
