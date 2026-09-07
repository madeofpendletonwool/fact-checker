// Package factcheck will hold the verification gate for generated or audited
// statements: deterministic spot-checks that always run (even offline), plus
// an entailment pass that judges each statement only against its own
// provenance.
//
// Failed statements are not machine-patched — a pluggable author hook
// rewrites them with the failure quotes as the editor's note, and the rewrite
// is re-checked as new content. Verdicts are keyed on statement id, prompt
// version, and content checksum, so unchanged settled statements are never
// billed twice.
package factcheck
