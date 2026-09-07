// Package validate will hold the quality gate between extraction and the
// claims graph: an adversarial second model re-checks every claim against its
// cited evidence, and a deterministic consistency engine (pure functions over
// a graph snapshot) catches what models miss.
//
// Machine passes may only downgrade, flag, or request rewrite — never
// upgrade, edit, or delete. Confidence moves down only, and only when
// strictly downward from the current value, so adversarial and deterministic
// passes compose safely.
package validate
