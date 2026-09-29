// Validation reports: flag rates, contradiction register, review queue.
//
// A run publishes these after every pass (disable by leaving ReportDir
// empty); markdown is for humans, JSON is the machine interface for the
// review surface. Ported from the reference implementation's
// validate/report.py.

package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Report file names.
const (
	FlagRatesJSON      = "flag_rates.json"
	FlagRatesMD        = "flag_rates.md"
	ContradictionsJSON = "contradictions.json"
	ContradictionsMD   = "contradictions.md"
	ReviewQueueJSON    = "review_queue.json"
	ReviewQueueMD      = "review_queue.md"
)

// FlagRatesReport is the machine-readable flag-rate payload.
type FlagRatesReport struct {
	RunID          int64                `json:"run_id"`
	Records        map[string]int       `json:"records"`
	Flags          FlagRatesFlags       `json:"flags"`
	Adversarial    FlagRatesAdversarial `json:"adversarial"`
	MachineActions MachineActionsStats  `json:"machine_actions"`
}

// FlagRatesFlags is the flags section of the report.
type FlagRatesFlags struct {
	Total             int                       `json:"total"`
	BySeverity        map[string]int            `json:"by_severity"`
	ByCheckCode       map[string]FlagRatesCheck `json:"by_check_code"`
	RatePer100Records map[string]float64        `json:"flag_rate_per_100_records"`
}

// FlagRatesCheck is one check's tally.
type FlagRatesCheck struct {
	Count    int    `json:"count"`
	Severity string `json:"severity"`
}

// FlagRatesAdversarial is the verdict summary section.
type FlagRatesAdversarial struct {
	Verdicts   VerdictSummary `json:"verdicts"`
	VerdictPct float64        `json:"verdict_rate"`
}

// WriteReports publishes the six report files and returns their names.
func WriteReports(ctx context.Context, pool *pgxpool.Pool, runID int64, dir string, stats *Stats) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("validate: create report dir: %w", err)
	}

	rates, err := BuildFlagRates(ctx, pool, runID, stats)
	if err != nil {
		return nil, err
	}
	register, err := ContradictionRegister(ctx, pool)
	if err != nil {
		return nil, err
	}
	queue, err := BuildReviewQueue(ctx, pool, runID)
	if err != nil {
		return nil, err
	}

	ratesJSON, err := json.MarshalIndent(rates, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("validate: marshal flag rates: %w", err)
	}
	registerJSON, err := json.MarshalIndent(register, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("validate: marshal contradictions: %w", err)
	}
	queueJSON, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("validate: marshal review queue: %w", err)
	}

	payloads := map[string]string{
		FlagRatesJSON:      string(ratesJSON),
		FlagRatesMD:        flagRatesMarkdown(rates),
		ContradictionsJSON: string(registerJSON),
		ContradictionsMD:   contradictionsMarkdown(register),
		ReviewQueueJSON:    string(queueJSON),
		ReviewQueueMD:      reviewQueueMarkdown(queue),
	}
	written := []string{}
	for _, name := range []string{FlagRatesJSON, FlagRatesMD, ContradictionsJSON, ContradictionsMD, ReviewQueueJSON, ReviewQueueMD} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(payloads[name]+"\n"), 0o644); err != nil {
			return written, fmt.Errorf("validate: write report %s: %w", path, err)
		}
		written = append(written, name)
	}
	return written, nil
}

// BuildFlagRates assembles the flag-rate payload for one run.
func BuildFlagRates(ctx context.Context, pool *pgxpool.Pool, runID int64, stats *Stats) (*FlagRatesReport, error) {
	flags, err := RunFlags(ctx, pool, runID)
	if err != nil {
		return nil, err
	}
	counts, err := RecordCounts(ctx, pool)
	if err != nil {
		return nil, err
	}
	verdicts, err := RunVerdictSummary(ctx, pool, runID)
	if err != nil {
		return nil, err
	}

	report := &FlagRatesReport{
		RunID:   runID,
		Records: counts,
		Flags: FlagRatesFlags{
			Total:             len(flags),
			BySeverity:        map[string]int{SeverityError: 0, SeverityWarning: 0, SeverityReview: 0},
			ByCheckCode:       map[string]FlagRatesCheck{},
			RatePer100Records: map[string]float64{},
		},
		Adversarial: FlagRatesAdversarial{Verdicts: verdicts},
	}
	perKind := map[string]int{}
	for _, flag := range flags {
		entry := report.Flags.ByCheckCode[flag.CheckCode]
		entry.Count++
		entry.Severity = flag.Severity
		report.Flags.ByCheckCode[flag.CheckCode] = entry
		report.Flags.BySeverity[flag.Severity]++
		perKind[flag.RecordKind]++
	}
	kindBases := map[string]int{
		"claim": counts["claims"], "event": counts["events"],
		"entity": counts["entities"], "relationship": counts["relationships"],
	}
	for kind, count := range perKind {
		base := kindBases[kind]
		rate := 0.0
		if base > 0 {
			rate = float64(count) / float64(base) * 100
		}
		report.Flags.RatePer100Records[kind] = roundTo(rate, 2)
	}
	report.Adversarial.VerdictPct = 0.0
	if counts["claims"] > 0 {
		report.Adversarial.VerdictPct = roundTo(float64(verdicts.Total)/float64(counts["claims"])*100, 2)
	}
	if stats != nil {
		report.MachineActions = stats.Machine
	}
	return report, nil
}

// QueueItem is one open finding a human should look at.
type QueueItem struct {
	CheckCode       string         `json:"check_code"`
	RecordKind      string         `json:"record_kind"`
	RecordID        string         `json:"record_id"`
	Severity        string         `json:"severity"`
	Message         string         `json:"message"`
	Details         map[string]any `json:"details"`
	ReviewStatus    string         `json:"review_status"`
	SuggestedAction string         `json:"suggested_action"`
}

// BuildReviewQueue lists this run's open findings, most severe first.
func BuildReviewQueue(ctx context.Context, pool *pgxpool.Pool, runID int64) ([]QueueItem, error) {
	flags, err := RunFlags(ctx, pool, runID)
	if err != nil {
		return nil, err
	}
	queue := []QueueItem{}
	for _, flag := range flags {
		if flag.ReviewStatus != "open" {
			continue
		}
		queue = append(queue, QueueItem{
			CheckCode:       flag.CheckCode,
			RecordKind:      flag.RecordKind,
			RecordID:        flag.RecordID,
			Severity:        flag.Severity,
			Message:         flag.Message,
			Details:         flag.Details,
			ReviewStatus:    flag.ReviewStatus,
			SuggestedAction: SuggestAction(flag.CheckCode),
		})
	}
	order := map[string]int{SeverityError: 0, SeverityReview: 1, SeverityWarning: 2}
	sort.SliceStable(queue, func(a, b int) bool {
		rankA, okA := order[queue[a].Severity]
		if !okA {
			rankA = 9
		}
		rankB, okB := order[queue[b].Severity]
		if !okB {
			rankB = 9
		}
		if rankA != rankB {
			return rankA < rankB
		}
		if queue[a].CheckCode != queue[b].CheckCode {
			return queue[a].CheckCode < queue[b].CheckCode
		}
		return queue[a].RecordID < queue[b].RecordID
	})
	return queue, nil
}

// SuggestAction names the human next step for a check code.
func SuggestAction(checkCode string) string {
	suggestions := map[string]string{
		"duplicate_claim":            "confirm merge or keep distinct; record the decision",
		"predicate_value_conflict":   "review the versions; keep both or resolve via human review",
		"adversarial_review":         "check the claim against its reference passage",
		"adversarial_downgrade":      "accept the downgrade or restore via human review",
		"established_without_tier1":  "add a Tier-1 citation or accept the downgrade",
		"claim_shape_invalid":        "re-extract with a machine-checkable structure",
		"missing_reference_citation": "add the reference citation or re-extract",
		"missing_reference_text":     "ingest the reference article, then re-run",
		"dangling_reference":         "restore the missing entity/event",
	}
	if suggestion, ok := suggestions[checkCode]; ok {
		return suggestion
	}
	return "review by hand"
}

func flagRatesMarkdown(report *FlagRatesReport) string {
	lines := []string{
		"# Validation flag-rate report",
		"",
		fmt.Sprintf("Run: %d", report.RunID),
		"",
		"## Corpus",
	}
	for _, kind := range sortedIntKeys(report.Records) {
		lines = append(lines, fmt.Sprintf("- %s: %d", kind, report.Records[kind]))
	}
	lines = append(lines,
		"",
		"## Flags",
		fmt.Sprintf("- total: %d", report.Flags.Total),
		fmt.Sprintf("- error: %d", report.Flags.BySeverity[SeverityError]),
		fmt.Sprintf("- warning: %d", report.Flags.BySeverity[SeverityWarning]),
		fmt.Sprintf("- review: %d", report.Flags.BySeverity[SeverityReview]),
		"",
		"Flag rate per 100 records:")
	for _, kind := range sortedFloatKeys(report.Flags.RatePer100Records) {
		lines = append(lines, fmt.Sprintf("- %s: %g", kind, report.Flags.RatePer100Records[kind]))
	}
	lines = append(lines, "", "## By check", "| check | severity | count |", "| ----- | -------- | ----- |")
	for _, code := range sortedCheckKeys(report.Flags.ByCheckCode) {
		entry := report.Flags.ByCheckCode[code]
		lines = append(lines, fmt.Sprintf("| %s | %s | %d |", code, entry.Severity, entry.Count))
	}
	lines = append(lines,
		"",
		"## Adversarial second model",
		fmt.Sprintf("- verdicts recorded: %d (%g%% of claims)",
			report.Adversarial.Verdicts.Total, report.Adversarial.VerdictPct))
	for _, verdict := range []string{"agree", "downgrade", "flag_review"} {
		if tally, ok := report.Adversarial.Verdicts.ByVerdict[verdict]; ok && tally.Count > 0 {
			lines = append(lines, fmt.Sprintf("- %s: %d (avg agreement %.2f)", verdict, tally.Count, tally.AvgAgreement))
		}
	}
	actions := report.MachineActions
	lines = append(lines,
		"",
		"## Machine actions",
		fmt.Sprintf("- claims contested: %d", actions.ClaimsContested),
		fmt.Sprintf("- contradictions registered (new): %d", actions.ContradictionsCreated),
		fmt.Sprintf("- contradiction entries reused: %d", actions.ContradictionsReused),
		fmt.Sprintf("- downgrades applied: %s", marshalOrEmpty(actions.DowngradesApplied)),
		"",
	)
	return strings.Join(lines, "\n")
}

func contradictionsMarkdown(register []ContradictionEntry) string {
	lines := []string{"# Contradiction register", ""}
	if len(register) == 0 {
		lines = append(lines, "(empty — no registered divergences)")
		return strings.Join(lines, "\n")
	}
	for _, entry := range register {
		lines = append(lines,
			fmt.Sprintf("## %s (`%s`) — %s", entry.Title, entry.ID, entry.Status),
			"",
			entry.Description,
			"")
		if entry.Resolution != "" {
			lines = append(lines, fmt.Sprintf("Resolution (human review): %s", entry.Resolution), "")
		}
		for _, claim := range entry.Claims {
			title := fmt.Sprintf("### %s — %s", claim.ClaimID, claim.Confidence)
			if claim.Tradition != "" {
				title += fmt.Sprintf(" (%s)", claim.Tradition)
			}
			lines = append(lines, title, "", claim.Statement, "")
			for _, version := range claim.Versions {
				sources := strings.Join(version.Sources, ", ")
				if sources == "" {
					sources = "(uncited)"
				}
				lines = append(lines, fmt.Sprintf("- **%s** [%s]: %s", version.Label, sources, version.Statement))
			}
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

func reviewQueueMarkdown(queue []QueueItem) string {
	lines := []string{"# Review queue", ""}
	if len(queue) == 0 {
		lines = append(lines, "(empty — no open review items)")
		return strings.Join(lines, "\n")
	}
	labels := map[string]string{SeverityError: "ERROR", SeverityWarning: "WARN", SeverityReview: "REVIEW"}
	for _, item := range queue {
		label, ok := labels[item.Severity]
		if !ok {
			label = strings.ToUpper(item.Severity)
		}
		lines = append(lines, fmt.Sprintf("- **[%s] %s** — %s `%s`: %s _(%s)_",
			label, item.CheckCode, item.RecordKind, item.RecordID, item.Message, item.SuggestedAction))
	}
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

func sortedIntKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedFloatKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedCheckKeys(m map[string]FlagRatesCheck) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func roundTo(value float64, places int) float64 {
	factor := 1.0
	for i := 0; i < places; i++ {
		factor *= 10
	}
	return float64(int64(value*factor+0.5)) / factor
}

func marshalOrEmpty(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}
