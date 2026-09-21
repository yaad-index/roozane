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
//
// It is 4 because the report now carries each source's zero-yield streak, so a
// source that has quietly stopped producing is a row someone can read rather
// than an absence nobody can see.
//
// It was 3 for an edition recording what its title pass did — how many
// headlines it offered, how many came back, how many it changed, and why it
// could not be completed — so a pass that did nothing stopped being the same
// record as a pass that had nothing to do, or as one that failed outright.
//
// Additive, so an existing reader keeps working — bumped anyway on the same
// grounds as DigestSchema: a version whose shape changed underneath a reader
// tells that reader nothing.
const ReportSchema = 4

// Pass names the calls a run makes, used to attribute spend.
//
// PassTitle runs at most once per edition and only when that edition names a
// language, so its line is absent from most reports rather than zero — the
// ledger records what was paid for, and a pass that never ran was not.
//
// PassGroup is the same shape: once per edition, and only when that edition
// configures a subject share (ADR-0007). Its absence from a report says the
// balance pass did not run, which for an unconfigured edition is the correct
// and expected outcome.
const (
	PassEnrich = "enrich"
	PassSelect = "select"
	PassGroup  = "group"
	PassTitle  = "title"
	PassDigest = "digest"
)

// Absence reasons a report can give for an item an edition did not carry.
//
// 🚨 There is deliberately no reason for "no source covers this topic at all".
// That absence is invisible by construction — the item never entered the
// pipeline, and no amount of reporting can describe what was never fetched. The
// report prints per-source yield so the gap is legible, and says plainly that
// these reasons do not explain every absence.
//
// Enrichment failure is deliberately not among them. It happens before any
// edition sees the item, so it is not a per-edition absence: ADR-0005 §7 answers
// that case at item level, where the report records StatusFailed with the error.
//
// 🚨 ReasonSelectFailed is a distinct answer from ReasonNotSelected and must
// never be folded into it. "Not selected" asserts that this edition's profile
// was applied and returned no; a failed select means the item was never judged
// at all. The two are opposite in what they say about the profile — one is
// evidence the profile works, the other is the absence of any evidence — and
// they are indistinguishable once written to the same string. Recording an
// unjudged item as "not selected by this edition's profile" would be a false
// statement in the one artifact whose job is to explain the day.
// 🚨 ReasonDigestFull and ReasonSubjectShare are likewise distinct from each other
// (ADR-0008 §6), and this is the pair most likely to be folded by someone tidying:
// both mean "selected but not carried", and to a reader of the digest they look
// identical. They answer different questions. A rising share count says this
// reader's source list has tilted towards one subject, which is about the
// configuration; a rising full count says there was more news than the configured
// length admits, which is about the length. The same string for both answers
// neither, and a short digest becomes unexplainable rather than explained.
//
// 🚨 ReasonSubjectShare is likewise distinct from ReasonNotSelected, and the
// distinction carries a diagnosis nothing else in the report offers. "Not
// selected" says the profile was applied and said no. This one says the profile
// said YES and the item lost its place to others on the same subject — so a
// rising count of it says this reader's source list is lopsided, which is a
// statement about the configuration rather than about the day. Folded into
// "not selected", that signal is destroyed and the report reads as though the
// profile rejected items it in fact chose.
const (
	ReasonBelowFloor   = "below the generic salience floor"
	ReasonNotInSources = "not in this edition's source list"
	ReasonNotSelected  = "not selected by this edition's profile"
	ReasonSelectFailed = "the selection call failed, so this item was never judged"
	ReasonSubjectShare = "selected, then crowded out by its own subject's share of the digest"
	ReasonDigestFull   = "selected, then left out because the digest reached its length"
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

	// Error carries what went wrong, for the absences caused by a failure
	// rather than by a judgement. It is empty for the ordinary reasons, which
	// are outcomes and not faults.
	//
	// It is carried because a reason alone does not survive the thing this
	// field exists for: an operator reading "the selection call failed" still
	// has to go to the logs for the cause, and a log is exactly where this
	// class of failure hid in the first place.
	Error string `json:"error,omitempty"`
}

// ReportEdition is one edition's outcome.
//
// ⚠️ These counts are in ARTICLES, while the digest the same run produced is
// written in EVENTS. The writing pass merges several reports of one occurrence
// into a single entry (see digestSystemPrompt), and nothing upstream of it
// knows that happened: the articles are still collected, still enriched, still
// selected, and still counted here individually.
//
// So a day where four articles covered one breach reports Selected as four and
// shows the reader one entry, and the two are both correct about different
// things. Anyone reconciling the report against the digest will hit this seam,
// which is why it is written here rather than left to be rediscovered from a
// report that looks wrong.
//
// Closing it means grouping before selection, so an event is the unit the whole
// pipeline counts. That inserts a stage into the pipeline ADR-0005 fixes, and
// the grouping is a property of a SET of items, which the enrichment cache
// cannot express — it is keyed on the item filename precisely because a neutral
// result is the same result for everybody (ADR-0005 §1). It therefore needs an
// ADR of its own, and this comment is the evidence for writing one.
type ReportEdition struct {
	ID         string          `json:"id"`
	Candidates int             `json:"candidates"`
	Selected   []string        `json:"selected"`
	Absent     []ReportAbsence `json:"absent"`
	Empty      bool            `json:"empty"`
	Failed     string          `json:"failed,omitempty"`

	// Titles is what the title pass did for this edition, absent when it was
	// never attempted. See TitleCounts.
	Titles *TitleCounts `json:"titles,omitempty"`

	// TitlesFailed carries why the title pass could not be completed, for an
	// edition that names a language.
	//
	// 🚨 It is here as well as on the digest because the counts alone cannot
	// separate the two: a pass that failed and a reply that named no headline
	// both record offered N, returned 0, changed 0. The digest carries the
	// cause for the reader; a report that omitted it would send its owner to
	// the digest to find out what the engine did, which is the direction ADR-0005
	// §7 draws the other way round.
	TitlesFailed string `json:"titles_failed,omitempty"`
}

// TitleCounts records what the title pass DID to an edition's headlines, rather
// than only that it ran.
//
// 🚨 The states it separates are otherwise one observation, and the spend
// row cannot separate them either: ten completion tokens is what a decline and a
// correct no-op both cost.
//
//   - ABSENT: the pass was never attempted — the edition names no language, or
//     no selected item carried a headline to offer.
//   - present, Returned 0: the pass ran and the reply named no headline at all.
//   - present, Returned > 0, Changed 0: the reply named them and they needed
//     nothing, which is the correct outcome for an edition whose sources
//     already publish in its language.
//   - present, Returned 0, alongside ReportEdition.TitlesFailed: the pass was
//     attempted and could not be completed.
//
// ⚠️ A pass that failed and a reply that named nothing do not separate on the
// counts alone — both are offered N, returned 0, changed 0 — which is why the
// cause is carried beside them rather than left to the digest.
//
// A bare "the pass ran" flag renders every attempted case as one record, and a
// bare changed-count does no better, since each attempted case reports zero
// changed. The case worth catching is the reply that named nothing, because a
// reply that named them all and changed none produces an identical zero
// legitimately (ADR-0005 §7's rule that an absence must not read as a quiet
// correct outcome).
//
// ⚠️ Which headlines changed is deliberately not repeated here. The digest
// already carries title_translated per item, set only where the pass changed
// something, so a per-headline list in the report would be a second copy that
// can disagree with the first.
type TitleCounts struct {
	// Offered is how many headlines were sent to the pass.
	Offered int `json:"offered"`

	// Returned is how many the reply named with a usable headline, whether or
	// not it differed from the original. It is the pass's only positive signal
	// that it engaged with the headlines at all.
	Returned int `json:"returned"`

	// Changed is how many headlines were actually put into the edition's
	// language. A headline returned identical is not counted — it needed
	// nothing — so Offered minus Changed is what was left alone.
	Changed int `json:"changed"`
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

	// Silence is every configured source's zero-yield streak, not only the
	// flagged ones. Sources carries what happened TODAY, which cannot answer
	// the question #10 asks: today's zero is the same row whether it is the
	// first or the fortieth.
	Silence []SourceSilence `json:"silence"`

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

// countAbsent counts the absences given for one reason.
func countAbsent(absences []ReportAbsence, reason string) int {
	n := 0
	for _, a := range absences {
		if a.Reason == reason {
			n++
		}
	}
	return n
}

// renderReport writes the operator-facing markdown.
// renderSilence is one source's streak in a sentence.
//
// ⚠️ An unevaluable row must not render as a number and nothing else. "2 empty
// runs" and "at least 2 empty runs, and the record stops there" are the two
// states this whole feature exists to separate, and the markdown is where an
// operator actually reads them — rendering both as "2" would put the bug back
// in the one artifact built to show it.
func renderSilence(silence SourceSilence) string {
	switch silence.Status {
	case SilenceCrossed:
		return fmt.Sprintf("FLAGGED: %s in a row, threshold %d (last produced items on %s)",
			plural(silence.Runs, "empty run"), silence.Threshold, lastYieldOrNever(silence))

	case SilenceUnevaluable:
		// A source with no run at all in the record is the ordinary state of a
		// freshly added one, and "at least 0 empty runs" is a true sentence
		// that tells nobody that.
		if silence.Runs == 0 {
			return fmt.Sprintf("no run of this source in the record, so there is nothing to judge%s — %s",
				thresholdClause(silence), walkClause(silence))
		}
		if silence.Threshold == 0 {
			return fmt.Sprintf("%s seen and none that produced anything%s — %s",
				plural(silence.Runs, "empty run"), thresholdClause(silence), walkClause(silence))
		}
		return fmt.Sprintf("%s seen and none that produced anything, short of the %d needed to judge — %s",
			plural(silence.Runs, "empty run"), silence.Threshold, walkClause(silence))

	default:
		return fmt.Sprintf("%s in a row%s (last produced items on %s)",
			plural(silence.Runs, "empty run"), thresholdClause(silence), lastYieldOrNever(silence))
	}
}

// walkClause is how far back the walk got, against how far it was allowed to.
//
// ⚠️ Both numbers or neither. "2 days of record" is equally consistent with a
// window that stops there and with an engine that has run for two days, and
// those call for opposite actions — raise retention, or wait — so printing the
// figure alone hands the reader a diagnosis they cannot actually make.
//
// 🚨 It is CONTEXT and never the headline, which an earlier version got wrong.
// These are properties of the walk, not of the source: a source added two days
// ago into a full record makes them read "the window stops here" and sends
// someone to widen a window that is already wide enough. The runs figure leads
// the sentence because the runs figure is the one about this source.
func walkClause(silence SourceSilence) string {
	return fmt.Sprintf("the walk covered %d of %s", silence.RecordDays, plural(silence.RetentionDays, "day"))
}

// thresholdClause is the trailing "threshold N", or the opt-out when flagging
// is off.
//
// ⚠️ It is one function rather than a clause repeated in each branch because
// silence_after 0 must not render as "threshold 0": there is no threshold, and
// a reader told there is one will reasonably conclude the source crossed it on
// its first empty run and that the flag is broken.
func thresholdClause(silence SourceSilence) string {
	if silence.Threshold == 0 {
		return ", never flagged (silence_after 0)"
	}
	return fmt.Sprintf(", threshold %d", silence.Threshold)
}

func lastYieldOrNever(silence SourceSilence) string {
	if silence.LastYield == "" {
		return "no day in the record"
	}
	return silence.LastYield
}

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

	b.WriteString("\n## Source silence\n\n")
	if len(report.Silence) == 0 {
		b.WriteString("_No sources configured._\n")
	} else {
		for _, silence := range report.Silence {
			fmt.Fprintf(&b, "- **%s** — %s\n", silence.Source, renderSilence(silence))
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
		// Counted from the absences rather than carried as its own field, so
		// the number and the per-item reasons cannot disagree.
		if unjudged := countAbsent(edition.Absent, ReasonSelectFailed); unjudged > 0 {
			fmt.Fprintf(&b, ", %d never judged (the selection call failed)", unjudged)
		}
		if edition.Empty {
			b.WriteString(" (empty)")
		}
		b.WriteString("\n\n")
		for _, name := range edition.Selected {
			fmt.Fprintf(&b, "- selected `%s`\n", name)
		}
		for _, absence := range edition.Absent {
			if absence.Error != "" {
				fmt.Fprintf(&b, "- `%s` — %s: %s\n", absence.Item, absence.Reason, absence.Error)
				continue
			}
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
