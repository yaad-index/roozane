package aggregate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/roozane/internal/store"
)

// item builds one selected item with the three fields the ceiling orders by.
func item(filename string, score, salience float64) selectedItem {
	return selectedItem{
		Item:       store.StoredItem{Filename: filename, Source: "src", Title: filename},
		Enrichment: Enrichment{Salience: salience},
		Selection:  Selection{Selected: true, Score: score},
	}
}

// names lists what survived, which is what the assertions are actually about.
func names(items []selectedItem) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, s.Item.Filename)
	}
	return out
}

func droppedNames(items []droppedItem) []string {
	out := make([]string, 0, len(items))
	for _, d := range items {
		out = append(out, d.Item.Item.Filename)
	}
	return out
}

// TestShareAllowanceIsComputedFromTheWholeSet pins the arithmetic ADR-0007 §3
// fixes, including the floor that keeps a subject alive.
func TestShareAllowanceIsComputedFromTheWholeSet(t *testing.T) {
	assert.Equal(t, 2, shareAllowance(8, 0.25))
	assert.Equal(t, 2, shareAllowance(11, 0.25), "the allowance rounds down rather than up")

	// The floor. Below four items a quarter is less than one item, and a
	// ceiling that rounded honestly there would erase every subject it touched.
	assert.Equal(t, 1, shareAllowance(3, 0.25))
	assert.Equal(t, 1, shareAllowance(1, 0.25))
	assert.Equal(t, 1, shareAllowance(0, 0.25), "an empty set still yields a usable allowance rather than zero")
}

// TestCeilingDoesNotRecomputeItsOwnAllowance is the test for the trap the ADR
// spends a section on, and it is written as an OUTCOME rather than as a check
// on an internal number, because the bug it guards against is a plausible later
// "fix" rather than a typo.
//
// Eight items, six of them one subject, a quarter each. The allowance is two, so
// six become two and the digest is four items — of which the big subject is now
// HALF. Recomputing the allowance against those four would give one, drop two
// more, and keep going: the loop terminates at an empty digest.
//
// So this asserts the survivors, and it asserts the resulting share is ABOVE the
// configured one. That second assertion looks wrong at a glance, which is why
// the reasoning is here: the configured share is the allowance's denominator,
// not a promise about the finished digest.
func TestCeilingDoesNotRecomputeItsOwnAllowance(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5), item("a3.md", 0.7, 0.5),
		item("a4.md", 0.6, 0.5), item("a5.md", 0.5, 0.5), item("a6.md", 0.4, 0.5),
		item("b1.md", 0.3, 0.5), item("b2.md", 0.2, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "one field", Members: []int{0, 1, 2, 3, 4, 5}},
		{Subject: "another", Members: []int{6, 7}},
	}

	kept, dropped := applyShareCeiling(selected, groups, 0.25)

	assert.Equal(t, []string{"a1.md", "a2.md", "b1.md", "b2.md"}, names(kept))
	assert.Equal(t, []string{"a3.md", "a4.md", "a5.md", "a6.md"}, droppedNames(dropped))

	// The whole point, stated as arithmetic: a second pass would find the big
	// subject at half and cut it again.
	assert.Equal(t, 2, shareAllowance(len(selected), 0.25))
	assert.Equal(t, 1, shareAllowance(len(kept), 0.25),
		"recomputing against the survivors lowers the allowance, which is why it must not be recomputed")
}

// TestSingleSubjectDayDropsNothing is ADR-0007 §4. With one subject the share is
// 1.0 whatever survives, so trimming changes no proportion and only costs the
// reader facts.
//
// ⚠️ This is also the case a naive implementation destroys most thoroughly: the
// share never falls below the cap however much it drops, so a loop empties the
// digest completely on precisely the day the reader's complaint was about.
func TestSingleSubjectDayDropsNothing(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5), item("a3.md", 0.7, 0.5),
		item("a4.md", 0.6, 0.5), item("a5.md", 0.5, 0.5), item("a6.md", 0.4, 0.5),
	}
	groups := []subjectGroup{{Subject: "one field", Members: []int{0, 1, 2, 3, 4, 5}}}

	kept, dropped := applyShareCeiling(selected, groups, 0.25)

	assert.Equal(t, names(selected), names(kept), "a single-subject day is carried whole")
	assert.Empty(t, dropped)
}

// TestSubjectIsNeverErased covers the floor from the outside: three subjects and
// a set too small for a quarter to be a whole item.
func TestSubjectIsNeverErased(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("b1.md", 0.8, 0.5), item("c1.md", 0.7, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "a", Members: []int{0}},
		{Subject: "b", Members: []int{1}},
		{Subject: "c", Members: []int{2}},
	}

	kept, dropped := applyShareCeiling(selected, groups, 0.25)

	assert.Equal(t, []string{"a1.md", "b1.md", "c1.md"}, names(kept),
		"floor(0.25*3) is zero; without the floor every subject would lose its only item")
	assert.Empty(t, dropped)
}

// TestSurvivorsAreTheStrongestMatches pins the ordering rule, one tier at a
// time, because a tie-break that is never reached is untested rather than
// correct.
func TestSurvivorsAreTheStrongestMatches(t *testing.T) {
	t.Run("score decides", func(t *testing.T) {
		selected := []selectedItem{
			item("low.md", 0.1, 0.9), item("high.md", 0.9, 0.1),
			item("other1.md", 0.5, 0.5), item("other2.md", 0.5, 0.5),
		}
		groups := []subjectGroup{
			{Subject: "a", Members: []int{0, 1}},
			{Subject: "b", Members: []int{2, 3}},
		}

		kept, dropped := applyShareCeiling(selected, groups, 0.25)

		// Allowance is one: the higher score survives even though the loser
		// carries the more substantive item.
		assert.Equal(t, []string{"high.md", "other1.md"}, names(kept))
		assert.Equal(t, []string{"low.md", "other2.md"}, droppedNames(dropped))
	})

	t.Run("salience breaks a score tie", func(t *testing.T) {
		// ⚠️ The filenames are chosen so the LAST tie-break disagrees with the
		// salience one. Named the obvious way round — "dense.md" against
		// "thin.md" — this test passed with the salience comparison deleted,
		// because the filename break happened to pick the same winner. It
		// asserted the right outcome for the wrong reason and could not fail.
		selected := []selectedItem{
			item("aaa-thin.md", 0.5, 0.2), item("zzz-dense.md", 0.5, 0.9),
			item("other1.md", 0.5, 0.5), item("other2.md", 0.5, 0.5),
		}
		groups := []subjectGroup{
			{Subject: "a", Members: []int{0, 1}},
			{Subject: "b", Members: []int{2, 3}},
		}

		kept, _ := applyShareCeiling(selected, groups, 0.25)
		assert.Contains(t, names(kept), "zzz-dense.md")
		assert.NotContains(t, names(kept), "aaa-thin.md")
	})

	t.Run("filename breaks a full tie, so a re-run is not a reshuffle", func(t *testing.T) {
		selected := []selectedItem{
			item("zzz.md", 0.5, 0.5), item("aaa.md", 0.5, 0.5),
			item("other1.md", 0.5, 0.5), item("other2.md", 0.5, 0.5),
		}
		groups := []subjectGroup{
			{Subject: "a", Members: []int{0, 1}},
			{Subject: "b", Members: []int{2, 3}},
		}

		kept, _ := applyShareCeiling(selected, groups, 0.25)
		assert.Contains(t, names(kept), "aaa.md")
		assert.NotContains(t, names(kept), "zzz.md")
	})
}

// TestCeilingIsIndifferentToMemberOrder is the determinism property stated as
// something that can fail. A digest that changed because the grouping pass
// listed its members differently would make any comparison between two runs
// meaningless.
func TestCeilingIsIndifferentToMemberOrder(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.5, 0.5), item("a2.md", 0.5, 0.5), item("a3.md", 0.5, 0.5),
		item("b1.md", 0.5, 0.5), item("b2.md", 0.5, 0.5),
	}
	forward := []subjectGroup{
		{Subject: "a", Members: []int{0, 1, 2}},
		{Subject: "b", Members: []int{3, 4}},
	}
	reversed := []subjectGroup{
		{Subject: "b", Members: []int{4, 3}},
		{Subject: "a", Members: []int{2, 1, 0}},
	}

	first, _ := applyShareCeiling(selected, forward, 0.25)
	second, _ := applyShareCeiling(selected, reversed, 0.25)

	assert.Equal(t, names(first), names(second))
	// Every field the ordering reads is equal here, so only the filename
	// break can be producing this answer.
	assert.Equal(t, []string{"a1.md", "b1.md"}, names(first))
}

// TestKeptItemsHoldTheirOriginalOrder guards against balancing quietly
// reordering the digest, which would be a second change hidden inside this one.
func TestKeptItemsHoldTheirOriginalOrder(t *testing.T) {
	selected := []selectedItem{
		item("b1.md", 0.9, 0.5), item("a1.md", 0.8, 0.5),
		item("b2.md", 0.7, 0.5), item("a2.md", 0.6, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "a", Members: []int{1, 3}},
		{Subject: "b", Members: []int{0, 2}},
	}

	kept, _ := applyShareCeiling(selected, groups, 0.25)

	// One from each subject, and they come back interleaved as they arrived
	// rather than gathered by subject.
	assert.Equal(t, []string{"b1.md", "a1.md"}, names(kept))
}

// TestDroppedItemsCarryTheirSubject is what lets the report say more than that
// an item is gone.
func TestDroppedItemsCarryTheirSubject(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5), item("a3.md", 0.7, 0.5),
		item("b1.md", 0.6, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "board games", Members: []int{0, 1, 2}},
		{Subject: "civic data", Members: []int{3}},
	}

	_, dropped := applyShareCeiling(selected, groups, 0.25)

	require.Len(t, dropped, 2)
	for _, d := range dropped {
		assert.Equal(t, "board games", d.Subject)
	}
}

// TestValidateGroupingRejectsAnythingThatIsNotAPartition is the safety property.
// Each case here is an item that would otherwise vanish from the digest with
// nothing recorded, or be counted twice.
func TestValidateGroupingRejectsAnythingThatIsNotAPartition(t *testing.T) {
	t.Run("an omitted item", func(t *testing.T) {
		err := validateGrouping([]subjectGroup{{Subject: "a", Members: []int{0, 2}}}, 3)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "item 1 was left out")
	})

	t.Run("a duplicated item", func(t *testing.T) {
		err := validateGrouping([]subjectGroup{
			{Subject: "a", Members: []int{0, 1}},
			{Subject: "b", Members: []int{1}},
		}, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "more than one subject")
	})

	t.Run("an index that is not an item", func(t *testing.T) {
		err := validateGrouping([]subjectGroup{{Subject: "a", Members: []int{0, 7}}}, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the 2 selected")
	})

	t.Run("a negative index", func(t *testing.T) {
		err := validateGrouping([]subjectGroup{{Subject: "a", Members: []int{-1}}}, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the 2 selected")
	})

	t.Run("an empty subject", func(t *testing.T) {
		err := validateGrouping([]subjectGroup{
			{Subject: "a", Members: []int{0, 1}},
			{Subject: "empty", Members: nil},
		}, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `subject "empty" holds no items`)
	})

	t.Run("no subjects at all", func(t *testing.T) {
		err := validateGrouping(nil, 3)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "named no subjects")
	})

	t.Run("a genuine partition", func(t *testing.T) {
		assert.NoError(t, validateGrouping([]subjectGroup{
			{Subject: "a", Members: []int{2, 0}},
			{Subject: "b", Members: []int{1}},
		}, 3))
	})
}

// TestEntriesSharingALabelDrawOneAllowance closes the hole where a subject
// returned as two entries collected the allowance twice.
//
// ⚠️ The grouping is a genuine partition in both shapes, so validateGrouping
// passes either way and nothing downstream says the ceiling did less than it was
// asked to. That is what makes it worth an assertion rather than a note: the
// under-bound digest is well-formed, and it fails in the direction of the
// complaint the ceiling exists to answer.
func TestEntriesSharingALabelDrawOneAllowance(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5),
		item("a3.md", 0.7, 0.5), item("a4.md", 0.6, 0.5),
		item("b1.md", 0.5, 0.5), item("b2.md", 0.4, 0.5),
		item("c1.md", 0.3, 0.5), item("c2.md", 0.2, 0.5),
	}
	split := []subjectGroup{
		{Subject: "one field", Members: []int{0, 1}},
		{Subject: "one field", Members: []int{2, 3}},
		{Subject: "another", Members: []int{4, 5}},
		{Subject: "a third", Members: []int{6, 7}},
	}
	whole := []subjectGroup{
		{Subject: "one field", Members: []int{0, 1, 2, 3}},
		{Subject: "another", Members: []int{4, 5}},
		{Subject: "a third", Members: []int{6, 7}},
	}

	fromSplit, droppedFromSplit := applyShareCeiling(selected, split, 0.25)
	fromWhole, _ := applyShareCeiling(selected, whole, 0.25)

	assert.Equal(t, names(fromWhole), names(fromSplit),
		"a subject returned as two entries is the same subject and gets one allowance")
	assert.Equal(t, []string{"a1.md", "a2.md", "b1.md", "b2.md", "c1.md", "c2.md"}, names(fromSplit))
	assert.Equal(t, []string{"a3.md", "a4.md"}, droppedNames(droppedFromSplit))
}

// TestMergingCanLeaveASingleSubject is the interaction between the two rules:
// once entries are folded, a day that looked like several subjects may be one,
// and a single-subject day is carried whole.
func TestMergingCanLeaveASingleSubject(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5),
		item("a3.md", 0.7, 0.5), item("a4.md", 0.6, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "one field", Members: []int{0, 1}},
		{Subject: "one field", Members: []int{2, 3}},
	}

	kept, dropped := applyShareCeiling(selected, groups, 0.25)

	assert.Equal(t, names(selected), names(kept))
	assert.Empty(t, dropped, "everything is one subject once the entries are folded")
}

// TestUnlabelledEntriesNeverMerge pins the direction the uncertain case fails
// in. A label is the only thing distinguishing these entries, so two unlabelled
// clusters may be unrelated — and merging them would drop items, where leaving
// them apart only leaves the digest less balanced.
func TestUnlabelledEntriesNeverMerge(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5),
		item("b1.md", 0.7, 0.5), item("b2.md", 0.6, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "", Members: []int{0, 1}},
		{Subject: "", Members: []int{2, 3}},
	}

	kept, dropped := applyShareCeiling(selected, groups, 0.25)

	assert.Equal(t, []string{"a1.md", "b1.md"}, names(kept),
		"two unlabelled clusters stay two subjects, each with its own allowance")
	assert.Len(t, dropped, 2)
}

func TestMergeSameSubjectKeepsFirstAppearanceOrderAndDoesNotAliasInput(t *testing.T) {
	// ⚠️ Two deliberate choices here, both learned by watching the test fail to
	// fail. The first entry's slice needs spare CAPACITY, or appending to it
	// reallocates and the caller's memory is spared by accident. And the check
	// has to read the BACKING ARRAY rather than the slice: an alias writes at
	// index 1, which is past the caller's length, so comparing the slice still
	// sees [0] and reports success while the memory underneath has changed.
	backing := []int{0, -1, -1, -1}
	first := backing[:1]

	groups := []subjectGroup{
		{Subject: "b", Members: first},
		{Subject: "a", Members: []int{1}},
		{Subject: "b", Members: []int{2}},
	}

	merged := mergeSameSubject(groups)

	require.Len(t, merged, 2)
	assert.Equal(t, "b", merged[0].Subject, "a folded subject keeps the position it first appeared in")
	assert.Equal(t, []int{0, 2}, merged[0].Members)
	assert.Equal(t, "a", merged[1].Subject)

	// The caller's grouping is untouched, so a merge cannot write into memory
	// something upstream still holds.
	assert.Equal(t, []int{0}, groups[0].Members)
	assert.Equal(t, -1, backing[1],
		"the merge must not write past the caller's slice into its backing array")
}
