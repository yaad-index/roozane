package aggregate

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/yaad-index/roozane/internal/collect"
	"github.com/yaad-index/roozane/internal/store"
)

// The three states a source's zero-yield streak can be in (#10).
//
// 🚨 They exist because two of them are otherwise one observation. A source
// with a streak of 2 against a threshold of 5 and a source with only 2 days of
// record behind it produce the same number, and one of them is a healthy source
// while the other is a question nobody has the evidence to answer. Rendering
// both as "2, not flagged" would rebuild the exact failure this feature exists
// to remove: an absence reading as health.
const (
	// SilenceEvaluated means a run that yielded items was actually observed, so
	// Runs is the real streak and not a floor.
	SilenceEvaluated = "evaluated"

	// SilenceCrossed means the streak reached the source's threshold. It is a
	// positive finding whether or not the record ran out afterwards: a streak
	// that is already long enough cannot be shortened by older days.
	SilenceCrossed = "crossed"

	// SilenceUnevaluable means the record ran out before either a yielding run
	// or the threshold was reached, so Runs is a LOWER BOUND and the source
	// cannot be judged. It is not a failure — a fresh install reports it for
	// every source and is behaving correctly.
	SilenceUnevaluable = "unevaluable"
)

// SourceSilence is one source's zero-yield streak as of this report.
//
// ⚠️ Every configured source gets a row, crossed or not. A list holding only
// the flagged ones would mean both "nothing is dark" and "the check never ran"
// with the same empty list, which is the shape of the bug rather than a fix for
// it — the same argument TitleCounts makes about a pass that did nothing.
type SourceSilence struct {
	Source string `json:"source"`

	// Status is one of the three constants above. It describes the certainty of
	// Runs, which is why a source is not flagged and unevaluable at once:
	// reaching the threshold settles the question on its own.
	Status string `json:"status"`

	// Runs is consecutive runs that yielded nothing, counted back from this
	// day. It is exact when Status is evaluated or crossed, and a floor when it
	// is unevaluable.
	//
	// A run is a day the source was actually collected. Days it was not due do
	// not extend the streak and do not end it: never attempted is not a quiet
	// run, and counting it as one would flag every weekly source six days in
	// seven.
	Runs int `json:"runs"`

	// Threshold is the source's silence_after, repeated per row so a row read
	// on its own can be judged. Zero means this source is never flagged, and
	// then Status is never crossed however long the streak.
	Threshold int `json:"threshold"`

	// LastYield is the day of the most recent run that produced items, empty
	// when no such run was found in the record. Empty alongside an evaluated
	// status cannot happen; empty alongside unevaluable is the ordinary case
	// for a source whose history is shorter than its silence.
	LastYield string `json:"last_yield,omitempty"`

	// RecordDays is how many consecutive days of collection record the walk
	// could read, ending at the first day with none.
	RecordDays int `json:"record_days"`

	// RetentionDays is the item-retention window RecordDays is measured
	// against, repeated per row for the same reason Threshold is: a row lifted
	// out on its own has to be judgeable.
	//
	// ⚠️ It is what makes an unevaluable row DIAGNOSABLE, and RecordDays alone
	// is not. Fourteen days of record is equally consistent with a window that
	// stops there and with an engine that has only run a fortnight, and those
	// call for opposite actions — raise retention, or wait. The comparison is
	// the whole diagnosis: RecordDays == RetentionDays means the window is the
	// binding constraint, and anything less means the history simply does not
	// go back that far yet.
	RetentionDays int `json:"retention_days"`
}

// sourceSilence walks the collection record backwards and reports each source's
// zero-yield streak.
//
// 🔑 The streak is DERIVED rather than stored. A counter incremented on each run
// would be a second copy of the truth that can drift from the record it
// describes, and would need its own reconciliation after a restore; a walk over
// the days cannot disagree with the days. It is affordable because a source is
// collected at most once per UTC day — due() compares whole UTC days — so one
// day's record holds at most one run per source and the walk is one small file
// per day.
func (r *Runner) sourceSilence(day time.Time) []SourceSilence {
	type tally struct {
		runs      int
		settled   bool
		lastYield string
	}

	tallies := make(map[string]*tally, len(r.cfg.Sources))
	for id := range r.cfg.Sources {
		tallies[id] = &tally{}
	}

	recordDays := 0
	// Nothing older than the item-retention window can exist, so the window is
	// the natural bound; the break below is what usually ends the walk.
	for age := 0; age < r.cfg.Retention.ItemDays(); age++ {
		on := day.AddDate(0, 0, -age)
		outcomes, ok := r.readCollected(on)
		if !ok {
			// No record for this day: the engine did not run, or the folder has
			// been pruned. Walking past it would count a streak across days
			// nothing was observed on, which is precisely the inference this
			// feature exists to stop anyone making.
			break
		}
		recordDays++

		for id, t := range tallies {
			if t.settled {
				continue
			}
			outcome, present := outcomes[id]
			switch {
			case !present || !outcome.Ran:
				// Not attempted, or not yet configured on that day. Neither a
				// quiet run nor a yielding one.
			case outcome.Items > 0:
				t.settled = true
				t.lastYield = store.Day(on)
			default:
				// Ran and produced nothing. The fetch may also have failed —
				// the streak counts it either way, because a source that is
				// broken is exactly as dark as one that is quiet, and the
				// error is already carried in the day's own outcome for
				// whoever needs to tell them apart.
				t.runs++
			}
		}
	}

	silences := make([]SourceSilence, 0, len(tallies))
	for _, id := range sortedSourceIDs(r.cfg.Sources) {
		t := tallies[id]
		threshold := r.cfg.Sources[id].SilenceThreshold()

		silence := SourceSilence{
			Source:        id,
			Runs:          t.runs,
			Threshold:     threshold,
			LastYield:     t.lastYield,
			RecordDays:    recordDays,
			RetentionDays: r.cfg.Retention.ItemDays(),
		}
		switch {
		case threshold > 0 && t.runs >= threshold:
			silence.Status = SilenceCrossed
		case t.settled:
			silence.Status = SilenceEvaluated
		default:
			silence.Status = SilenceUnevaluable
		}
		silences = append(silences, silence)
	}
	sort.Slice(silences, func(i, j int) bool { return silences[i].Source < silences[j].Source })
	return silences
}

// readCollected reads one day's collection record, reporting whether there was
// a usable one.
//
// ⚠️ It reports false for an unreadable file as well as a missing one, which
// ends the walk. That is the conservative direction: the alternative is to skip
// the day and join a streak across it, which would report a confident number
// built partly on a day nobody can read.
func (r *Runner) readCollected(day time.Time) (map[string]collect.SourceOutcome, bool) {
	raw, err := os.ReadFile(r.store.CollectedPath(day)) //nolint:gosec // path is inside the engine's own data root
	if err != nil {
		return nil, false
	}

	var outcomes collect.Outcomes
	if err := json.Unmarshal(raw, &outcomes); err != nil {
		r.log.Warn("collection outcomes unreadable; the silence walk stops here",
			"day", store.Day(day), "error", err)
		return nil, false
	}
	return outcomes.Sources, true
}
