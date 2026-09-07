// Package sources will hold the tiered source registry: sources, tiers and
// what each tier means (defined by the adapter), locator schemes (what a
// valid citation locator looks like for each source — chapter, URL, char
// range, file+line, commit), and licensing fields.
//
// Provenance is part of the data model: a claim that cannot cite a source is
// dropped, so the registry is the foundation everything else cites into.
package sources
