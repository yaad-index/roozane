package aggregate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yaad-index/roozane/internal/collect"
	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/store"
)

// ReportSchema versions reports/<day>.json.
const ReportSchema = 1

// Pass names the three calls a run makes, used to attribute spend.
const (
	PassEnrich = "enrich"
	PassSelect = "select"
	PassDigest = "digest"
)

// Absence reasons a report can give for an item an edition did not carry.
//
// 🚨 There is deliberately no reason for "no source covers this topic at all".
// That absence is invisible by construction — the item never entered the
// pipeline, and no amount of reporting can describe what was never fetched. The
// report prints per-source yield so the gap is legible, and says plainly that
// these reasons do not explain every absence.
const (
	ReasonEnrichFailed = "enrichment failed"
	ReasonBelowFloor   = "below the generic salience floor"
	ReasonNotInSources = "not in this edition's source list"
	ReasonNotSelected  = "not selected by this edition's profile"
)

// PassSpend is one pass's spend on one model, for this run.
type PassSpend struct {
	Pass  string `json:"pass"`
	Model string `json:"model"`
	Calls int    `json:"calls"`

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`

	// WallMillis is time spent inside the calls, not the pass's elapsed time.
	WallMillis int64 `json:"wall_millis"`

	// Cost is money at the configured rates, omitted when the model has no
	// entry. Priced is what tells those apart: a missing cost must never read
	// as a free one.
	Cost   float64 `json:"cost,omitempty"`
	Priced bool    `json:"priced"`

	// Currency repeats the configured denomination on every priced line. A cost
	// carried without its currency is a number the reader supplies a guess for,
	// and the guess is wrong by an exchange rate rather than visibly wrong.
	Currency string `json:"currency,omitempty"`
}

// ReportItem is what the neutral pass made of one item.
type ReportItem struct {
	Item     string   `json:"item"`
	Source   string   `json:"source"`
	Title    string   `json:"title,omitempty"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags,omitempty"`
	Category string   `json:"category,omitempty"`
	Salience float64  `json:"salience"`
	Error    string   `json:"error,omitempty"`
}

// ReportAbsence is one item an edition did not carry, and why.
type ReportAbsence struct {
	Item   string `json:"item"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// ReportEdition is one edition's outcome.
type ReportEdition struct {
	ID         string          `json:"id"`
	Candidates int             `json:"candidates"`
	Selected   []string        `json:"selected"`
	Absent     []ReportAbsence `json:"absent"`
	Empty      bool            `json:"empty"`
	Failed     string          `json:"failed,omitempty"`
}

// Report is `reports/<day>.json` (ADR-0005 §7): what the engine did today and
// why, written for its owner rather than for a reader.
type Report struct {
	Schema      int    `json:"schema"`
	Day         string `json:"day"`
	GeneratedAt string `json:"generated_at"`

	Sources  map[string]collect.SourceOutcome `json:"sources"`
	Items    []ReportItem                     `json:"items"`
	Editions []ReportEdition                  `json:"editions"`

	Spend []PassSpend `json:"spend"`

	// SpendIsPerRun is always true and is written anyway, because the figure it
	// qualifies reads like a bug without it: re-running a day reports near-zero
	// tokens, since a reused enrichment is not paid for again and so is not
	// counted. Summing a day across runs would double-count every re-run.
	SpendIsPerRun bool `json:"spend_is_per_run"`

	// UnpricedModels names models that were used and have no configured rate.
	// Their spend is real and simply unpriced, so a total that omitted them
	// silently would understate the day and look like a cheap one.
	UnpricedModels []string `json:"unpriced_models,omitempty"`
}

// spendKey identifies one pass-and-model pair.
type spendKey struct {
	pass  string
	model string
}

// spendLedger accumulates per-pass, per-model usage across a run.
type spendLedger struct {
	entries map[spendKey]*PassSpend
}

func newSpendLedger() *spendLedger {
	return &spendLedger{entries: map[spendKey]*PassSpend{}}
}

// record adds one paid call. A reused enrichment is never recorded, which is
// what makes the figures per-run rather than per-day.
func (l *spendLedger) record(pass, model string, promptTokens, completionTokens int, wall time.Duration) {
	key := spendKey{pass: pass, model: model}
	entry, ok := l.entries[key]
	if !ok {
		entry = &PassSpend{Pass: pass, Model: model}
		l.entries[key] = entry
	}
	entry.Calls++
	entry.PromptTokens += promptTokens
	entry.CompletionTokens += completionTokens
	entry.WallMillis += wall.Milliseconds()
}

// priced returns the ledger sorted, with costs applied where a rate exists, and
// the models that were used without one.
func (l *spendLedger) priced(prices config.Prices) ([]PassSpend, []string) {
	out := make([]PassSpend, 0, len(l.entries))
	for _, entry := range l.entries {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pass != out[j].Pass {
			return out[i].Pass < out[j].Pass
		}
		return out[i].Model < out[j].Model
	})

	if !prices.Configured() {
		return out, nil
	}

	unpriced := map[string]bool{}
	for i := range out {
		price, ok := prices.For(out[i].Model)
		if !ok {
			unpriced[out[i].Model] = true
			continue
		}
		out[i].Cost = price.Cost(out[i].PromptTokens, out[i].CompletionTokens)
		out[i].Priced = true
		out[i].Currency = prices.Currency
	}

	names := make([]string, 0, len(unpriced))
	for name := range unpriced {
		names = append(names, name)
	}
	sort.Strings(names)
	return out, names
}

// writeReport renders and writes both report files.
//
// It runs after every edition has been written, which is the one ordering this
// design imposes: the report describes what the editions selected, so it cannot
// precede them.
func (r *Runner) writeReport(day time.Time, report Report) error {
	report.Schema = ReportSchema
	report.Day = store.Day(day)
	report.GeneratedAt = r.now().UTC().Format(time.RFC3339)
	report.SpendIsPerRun = true

	structured, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}

	mdPath, jsonPath := r.store.ReportPaths(day)
	if err := r.store.WriteAtomic(mdPath, []byte(renderReport(report))); err != nil {
		return fmt.Errorf("write report markdown: %w", err)
	}
	if err := r.store.WriteAtomic(jsonPath, append(structured, '\n')); err != nil {
		return fmt.Errorf("write report json: %w", err)
	}
	return nil
}

// plural renders a count with its noun. The report is read by a person, and
// "1 items" is the kind of thing that makes a generated document feel like
// output rather than writing.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// renderReport writes the operator-facing markdown.
func renderReport(report Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Report — %s\n\n", report.Day)

	b.WriteString("## Sources\n\n")
	if len(report.Sources) == 0 {
		b.WriteString("_No collection record for this day._\n")
	} else {
		ids := make([]string, 0, len(report.Sources))
		for id := range report.Sources {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			outcome := report.Sources[id]
			switch {
			case !outcome.Ran:
				fmt.Fprintf(&b, "- **%s** — not due\n", id)
			case outcome.Error != "":
				fmt.Fprintf(&b, "- **%s** — FAILED after %s: %s\n", id, plural(outcome.Items, "item"), outcome.Error)
			default:
				fmt.Fprintf(&b, "- **%s** — %s\n", id, plural(outcome.Items, "item"))
			}
		}
	}

	// 🚨 The honest limit, in the output and not only in the ADR. Four of the
	// five reasons an item can be absent are recoverable and appear below; the
	// fifth — no source covers the topic at all — is invisible by construction.
	// The per-source yield above is what makes that gap legible, and this note
	// is what stops the absence lists reading as a complete account.
	b.WriteString("\n> The per-edition reasons below explain items that entered the pipeline. " +
		"They cannot explain a topic no configured source covers: such an item was never fetched, " +
		"so nothing here can describe it. Read the yields above for that.\n")

	b.WriteString("\n## Items\n\n")
	if len(report.Items) == 0 {
		b.WriteString("_No items collected for this day._\n")
	} else {
		for _, item := range report.Items {
			if item.Status == StatusFailed {
				fmt.Fprintf(&b, "- `%s` (%s) — ENRICHMENT FAILED: %s\n", item.Item, item.Source, item.Error)
				continue
			}
			line := fmt.Sprintf("- `%s` (%s) — salience %.2f", item.Item, item.Source, item.Salience)
			if item.Category != "" {
				line += ", " + item.Category
			}
			if len(item.Tags) > 0 {
				line += ", tags: " + strings.Join(item.Tags, " ")
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n## Editions\n\n")
	for _, edition := range report.Editions {
		fmt.Fprintf(&b, "### %s\n\n", edition.ID)
		if edition.Failed != "" {
			fmt.Fprintf(&b, "FAILED: %s\n\n", edition.Failed)
			continue
		}
		fmt.Fprintf(&b, "%s, %d selected", plural(edition.Candidates, "candidate"), len(edition.Selected))
		if edition.Empty {
			b.WriteString(" (empty)")
		}
		b.WriteString("\n\n")
		for _, name := range edition.Selected {
			fmt.Fprintf(&b, "- selected `%s`\n", name)
		}
		for _, absence := range edition.Absent {
			fmt.Fprintf(&b, "- `%s` — %s\n", absence.Item, absence.Reason)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Spend\n\n")
	if len(report.Spend) == 0 {
		b.WriteString("_Nothing was paid for on this run._\n")
	} else {
		for _, spend := range report.Spend {
			line := fmt.Sprintf("- **%s** / %s — %s, %d prompt + %d completion tokens, %dms",
				spend.Pass, spend.Model, plural(spend.Calls, "call"), spend.PromptTokens, spend.CompletionTokens, spend.WallMillis)
			if spend.Priced {
				// The currency is repeated on every figure, not printed once in
				// a header: a number read out of context otherwise carries a
				// denomination the reader supplies from memory.
				line += fmt.Sprintf(", %.4f %s", spend.Cost, spend.Currency)
			}
			b.WriteString(line + "\n")
		}
	}

	if len(report.UnpricedModels) > 0 {
		fmt.Fprintf(&b, "\n⚠️ No configured rate for: %s. Their spend is real and is not included in any cost above.\n",
			strings.Join(report.UnpricedModels, ", "))
	}

	// ⏱️ Stated because the figure reads like a bug otherwise: re-running a day
	// reports near-zero tokens, and that is correct.
	b.WriteString("\nThese figures are for THIS RUN, not for the day. " +
		"A reused enrichment costs nothing and is not counted, so re-running a day reports " +
		"near-zero spend — that is correct, and summing runs would double-count every re-run.\n")

	return b.String()
}
