package aggregate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/roozane/internal/collect"
	"github.com/yaad-index/roozane/internal/store"
)

// silenceFor pulls one source's row out of a report.
func silenceFor(t *testing.T, report Report, source string) SourceSilence {
	t.Helper()
	for _, silence := range report.Silence {
		if silence.Source == source {
			return silence
		}
	}
	require.Failf(t, "no silence row", "source %q is configured, so it must have a row", source)
	return SourceSilence{}
}

// TestEveryConfiguredSourceGetsARowEvenWhenNothingIsWrong is the shape the
// feature turns on: a list of only the flagged sources would be empty both when
// nothing is dark and when the check never ran.
func TestEveryConfiguredSourceGetsARowEvenWhenNothingIsWrong(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")
	writeCollected(t, root, day, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 2},
		"b-source": {Ran: true, Items: 1},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	_, report := readReport(t, root, day)

	require.Len(t, report.Silence, 2, "both configured sources are recorded, not only the flagged ones")
	for _, source := range []string{"a-source", "b-source"} {
		silence := silenceFor(t, report, source)
		assert.Equal(t, SilenceEvaluated, silence.Status)
		assert.Zero(t, silence.Runs)
		assert.Equal(t, store.Day(day), silence.LastYield)
	}
}

// TestTheStreakCrossesAtTheThreshold is the flag firing on a real streak.
func TestTheStreakCrossesAtTheThreshold(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")
	for age := range 3 {
		writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
			"a-source": {Ran: true, Items: 0},
			"b-source": {Ran: true, Items: 1},
		})
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	markdown, report := readReport(t, root, day)

	dark := silenceFor(t, report, "a-source")
	assert.Equal(t, SilenceCrossed, dark.Status)
	assert.Equal(t, 3, dark.Runs)
	assert.Equal(t, 3, dark.Threshold)
	assert.Contains(t, markdown, "**a-source** — FLAGGED")

	healthy := silenceFor(t, report, "b-source")
	assert.Equal(t, SilenceEvaluated, healthy.Status)
	assert.Zero(t, healthy.Runs)
}

// TestAShortRecordIsNotReportedAsAHealthySource is the distinction the whole
// three-state record exists for.
//
// 🚨 A streak of two against a threshold of three, and a streak of two with
// nothing older to read, are the same NUMBER and opposite facts: the first says
// the source is fine, the second says nobody can tell. Collapsing them puts the
// original bug — an absence reading as health — inside the feature built to
// remove it.
func TestAShortRecordIsNotReportedAsAHealthySource(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")
	for age := range 2 {
		writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
			"a-source": {Ran: true, Items: 0},
			"b-source": {Ran: true, Items: 1},
		})
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	markdown, report := readReport(t, root, day)

	unevaluable := silenceFor(t, report, "a-source")
	assert.Equal(t, SilenceUnevaluable, unevaluable.Status,
		"two empty runs with no older record cannot be called healthy: the walk ran out before the threshold")
	assert.Equal(t, 2, unevaluable.Runs, "the count is a floor, and is still worth reporting")
	assert.Equal(t, 2, unevaluable.RecordDays)
	assert.Equal(t, 90, unevaluable.RetentionDays,
		"the window is carried beside the record so the row can be diagnosed on its own")
	assert.Empty(t, unevaluable.LastYield)

	// The markdown is where an operator reads this, so the two states have to
	// differ THERE and not only in the JSON.
	assert.Contains(t, markdown, "at least 2 empty runs in a row and the record stops there")
	assert.NotContains(t, markdown, "**a-source** — 2 empty runs in a row, threshold 3")
}

// TestAYieldingRunEndsTheStreakExactly separates an evaluated short streak from
// the unevaluable one above: same count, and here it is known to be the truth.
func TestAYieldingRunEndsTheStreakExactly(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")
	for age := range 2 {
		writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
			"a-source": {Ran: true, Items: 0},
		})
	}
	yieldDay := day.AddDate(0, 0, -2)
	writeCollected(t, root, yieldDay, map[string]collect.SourceOutcome{
		"a-source": {Ran: true, Items: 4},
	})

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	_, report := readReport(t, root, day)

	silence := silenceFor(t, report, "a-source")
	assert.Equal(t, SilenceEvaluated, silence.Status)
	assert.Equal(t, 2, silence.Runs)
	assert.Equal(t, store.Day(yieldDay), silence.LastYield)
}

// TestADayTheSourceWasNotDueNeitherBreaksNorExtendsTheStreak keeps a weekly
// source from being flagged by the six days it was never asked to run.
func TestADayTheSourceWasNotDueNeitherBreaksNorExtendsTheStreak(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")

	// Ran on the odd ages only; not due on the others.
	for age := range 7 {
		outcome := collect.SourceOutcome{Ran: false}
		if age%2 == 1 {
			outcome = collect.SourceOutcome{Ran: true, Items: 0}
		}
		writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{"a-source": outcome})
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	_, report := readReport(t, root, day)

	silence := silenceFor(t, report, "a-source")
	assert.Equal(t, 3, silence.Runs, "ages 1, 3 and 5 ran and yielded nothing; the not-due days are not runs")
	assert.Equal(t, SilenceCrossed, silence.Status)
}

// TestAMissingDayStopsTheWalkRatherThanBeingSteppedOver keeps the count from
// spanning days nothing was observed on.
func TestAMissingDayStopsTheWalkRatherThanBeingSteppedOver(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")
	for _, age := range []int{0, 1, 3, 4} { // age 2 is missing: the engine did not run
		writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
			"a-source": {Ran: true, Items: 0},
		})
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	_, report := readReport(t, root, day)

	silence := silenceFor(t, report, "a-source")
	assert.Equal(t, 2, silence.Runs, "the walk stops at the gap: ages 3 and 4 are not joined onto 0 and 1")
	assert.Equal(t, 2, silence.RecordDays)
	assert.Equal(t, SilenceUnevaluable, silence.Status,
		"a streak interrupted by a day with no record is a floor, not a verdict")
}

// TestAThresholdOfZeroIsNeverFlagged is the opt-out: a source whose quiet
// periods are genuinely unbounded still reports its streak, and never crosses.
func TestAThresholdOfZeroIsNeverFlagged(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	// The fixture's sources block is already open, so this entry joins it.
	cfg, root := fixture(t, day, "profile",
		"  quiet-source: {collector: feed, cadence: daily, silence_after: 0}\n")
	for age := range 10 {
		writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
			"quiet-source": {Ran: true, Items: 0},
		})
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	markdown, report := readReport(t, root, day)

	silence := silenceFor(t, report, "quiet-source")
	assert.Equal(t, 10, silence.Runs, "the count is still reported: turning the flag off does not hide the data")
	assert.NotEqual(t, SilenceCrossed, silence.Status)
	assert.Zero(t, silence.Threshold)
	assert.Contains(t, markdown, "never flagged (silence_after 0)")
}

// TestAFailedFetchCountsTowardsTheStreak follows the issue: a broken source is
// exactly as dark as a quiet one, and the day's own outcome already carries the
// error for whoever needs to tell them apart.
func TestAFailedFetchCountsTowardsTheStreak(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "")
	for age := range 3 {
		writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
			"a-source": {Ran: true, Items: 0, Error: "dial tcp: connection refused"},
		})
	}

	_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
	require.NoError(t, err)
	_, report := readReport(t, root, day)

	assert.Equal(t, SilenceCrossed, silenceFor(t, report, "a-source").Status)
}

// TestAnUnevaluableRowSaysWhichConstraintIsBinding is what RetentionDays buys.
//
// 🚨 RecordDays alone cannot be acted on. "2 days of record" is equally the
// answer for a window that stops at 2 and for an engine that has run twice, and
// the two call for opposite responses — raise retention, or simply wait. Only
// the comparison distinguishes them, so both numbers travel together or the row
// states a diagnosis its reader cannot make.
func TestAnUnevaluableRowSaysWhichConstraintIsBinding(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")

	t.Run("retention is the binding constraint", func(t *testing.T) {
		// A two-day window under a threshold of three: the streak can never be
		// settled, and more record exists on disk than the window admits, so
		// the walk is stopped by retention rather than by a short history.
		cfg, root := fixture(t, day, "profile", "\nretention:\n  items: 2\n")
		for age := range 6 {
			writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
				"a-source": {Ran: true, Items: 0},
			})
		}

		_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
		require.NoError(t, err)
		markdown, report := readReport(t, root, day)

		silence := silenceFor(t, report, "a-source")
		assert.Equal(t, SilenceUnevaluable, silence.Status)
		assert.Equal(t, silence.RetentionDays, silence.RecordDays,
			"record equal to the window is what says retention is the thing that stopped the walk")
		assert.Contains(t, markdown, "2 of 2 days of record")
	})

	t.Run("the history is simply short", func(t *testing.T) {
		cfg, root := fixture(t, day, "profile", "")
		for age := range 2 {
			writeCollected(t, root, day.AddDate(0, 0, -age), map[string]collect.SourceOutcome{
				"a-source": {Ran: true, Items: 0},
			})
		}

		_, err := runner(t, cfg, &stubClient{}, day).Run(context.Background(), day)
		require.NoError(t, err)
		markdown, report := readReport(t, root, day)

		silence := silenceFor(t, report, "a-source")
		assert.Equal(t, SilenceUnevaluable, silence.Status)
		assert.Less(t, silence.RecordDays, silence.RetentionDays,
			"record short of the window says the engine has not run long enough, not that retention is wrong")
		assert.Contains(t, markdown, "2 of 90 days of record",
			"the operator reads the markdown, so both numbers have to be in the sentence")
	})
}
