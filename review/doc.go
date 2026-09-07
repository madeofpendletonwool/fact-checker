// Package review will hold the human review queue: the place only a person
// can go. Disputed, high-impact, or undecidable material — entailment
// failures that survived revision, interpretive additions, contested-as-settled
// usage, model disagreement — flows to a queue a human works without
// engineering help.
//
// Decisions are durable: a decided item keeps its decision forever, and a
// finding that reappears opens a new item rather than resurrecting the old
// one. Confidence upgrades and contradiction resolutions happen only here.
package review
