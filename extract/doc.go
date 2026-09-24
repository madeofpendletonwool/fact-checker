// Package extract holds the cite-or-drop extraction stage: turning
// ingested documents into atomic, fully-cited claims, entities, and
// events.
//
// The model extracts and cites; it never decides what is true. Outputs may
// only cite sources from the adapter-built candidate set for the work unit;
// anything uncited is dropped and logged with a structured reason, never
// kept. Prompts are versioned; the prompt version plus input hash is the
// ledger key that makes runs idempotent and re-extraction explicit.
package extract
