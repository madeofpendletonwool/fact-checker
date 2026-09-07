// Package adapters will define the domain contract that makes the engine a
// platform: the boundary between the generic pipeline and a domain.
//
// An adapter supplies a source registry (sources, tiers, locator schemes,
// licensing), an ingester (documents to work units plus citation candidate
// sets), an ontology (entity/relationship/event types), a deterministic check
// set, optional ground-truth connectors, and optional hooks (fact-check
// author, renderers, confidence-policy overrides). Adapters are declarative
// configuration plus Go code against the SDK contract; the engine never
// changes per domain. See ADR-0002.
package adapters
