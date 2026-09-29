// Package validate is the quality gate between extraction and the claims
// graph: an adversarial second model re-checks every claim against its
// cited evidence, and a deterministic consistency engine (pure functions
// over a graph snapshot, structured as a check registry adapters extend)
// catches what models miss.
//
// Machine passes may only downgrade, flag, or request rewrite — never
// upgrade, edit, or delete. Confidence moves down only, and only when
// strictly downward from the current value, so adversarial and
// deterministic passes compose safely. Contradictions between credible
// sources are preserved in the register with per-source versions, never
// averaged away or auto-resolved.
package validate
