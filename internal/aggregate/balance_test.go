package aggregate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/roozane/internal/config"
	"github.com/yaad-index/roozane/internal/llm"
	"github.com/yaad-index/roozane/internal/store"
)

// share is a pointer helper: the fill distinguishes an absent share from a
// configured one, and a bare literal cannot express the difference.
func share(v float64) *float64 { return &v }

// entries is the same for the digest length.
func entries(n int) *int { return &n }

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

	kept, dropped := fillDigest(selected, groups, share(0.25), nil)

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

	kept, dropped := fillDigest(selected, groups, share(0.25), nil)

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

	kept, dropped := fillDigest(selected, groups, share(0.25), nil)

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

		kept, dropped := fillDigest(selected, groups, share(0.25), nil)

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

		kept, _ := fillDigest(selected, groups, share(0.25), nil)
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

		kept, _ := fillDigest(selected, groups, share(0.25), nil)
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

	first, _ := fillDigest(selected, forward, share(0.25), nil)
	second, _ := fillDigest(selected, reversed, share(0.25), nil)

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

	kept, _ := fillDigest(selected, groups, share(0.25), nil)

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

	_, dropped := fillDigest(selected, groups, share(0.25), nil)

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

	fromSplit, droppedFromSplit := fillDigest(selected, split, share(0.25), nil)
	fromWhole, _ := fillDigest(selected, whole, share(0.25), nil)

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

	kept, dropped := fillDigest(selected, groups, share(0.25), nil)

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

	kept, dropped := fillDigest(selected, groups, share(0.25), nil)

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

// --- the grouping reply ---

// itemsAskedAbout counts the items a grouping request was given, by the numbered
// headings buildGroupMessages writes. It lets a stub answer a request it did not
// author, which is what keeps the default answer valid for any set size.
func itemsAskedAbout(req llm.Request) int {
	if len(req.Messages) < 2 {
		return 0
	}
	return strings.Count(req.Messages[1].Content, "\n## ")
}

// oneSubjectJSON is a valid partition putting every item in one subject.
func oneSubjectJSON(n int) string {
	items := make([]string, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, strconv.Itoa(i))
	}
	return `{"subjects": [{"subject": "everything", "items": [` + strings.Join(items, ", ") + `]}]}`
}

// splitSubjectsJSON puts the first n-tail items in one subject and the rest in
// another, which is the shape that makes a ceiling bind.
func splitSubjectsJSON(n, tail int) string {
	var head, rest []string
	for i := 0; i < n-tail; i++ {
		head = append(head, strconv.Itoa(i))
	}
	for i := n - tail; i < n; i++ {
		rest = append(rest, strconv.Itoa(i))
	}
	return `{"subjects": [
		{"subject": "the busy one", "items": [` + strings.Join(head, ", ") + `]},
		{"subject": "the other", "items": [` + strings.Join(rest, ", ") + `]}
	]}`
}

func TestParseGroupingReadsAPartition(t *testing.T) {
	t.Run("plain json", func(t *testing.T) {
		groups, err := parseGrouping(`{"subjects": [{"subject": "a", "items": [0, 2]}, {"subject": "b", "items": [1]}]}`, 3)
		require.NoError(t, err)
		require.Len(t, groups, 2)
		assert.Equal(t, "a", groups[0].Subject)
		assert.Equal(t, []int{0, 2}, groups[0].Members)
	})

	t.Run("fenced json", func(t *testing.T) {
		groups, err := parseGrouping("```json\n"+`{"subjects": [{"subject": "a", "items": [0]}]}`+"\n```", 1)
		require.NoError(t, err)
		require.Len(t, groups, 1)
	})

	t.Run("unnamed subjects get DISTINCT placeholders, so they are not merged together", func(t *testing.T) {
		// 🚨 Not cosmetic. mergeSameSubject folds entries sharing a label, so one
		// shared placeholder would merge clusters the grouping never said were
		// related — and merging two subjects into one drops items, where leaving
		// them apart only leaves the digest less balanced.
		groups, err := parseGrouping(`{"subjects": [{"subject": "  ", "items": [0]}, {"subject": "", "items": [1]}]}`, 2)
		require.NoError(t, err)
		require.Len(t, groups, 2)
		assert.Equal(t, "unnamed 1", groups[0].Subject)
		assert.Equal(t, "unnamed 2", groups[1].Subject)
		assert.NotEqual(t, groups[0].Subject, groups[1].Subject)

		// And the property that actually matters, asserted through the ceiling
		// rather than through the labels: two unlabelled clusters keep their own
		// allowances.
		selected := []selectedItem{item("a.md", 0.9, 0.5), item("b.md", 0.8, 0.5)}
		kept, dropped := fillDigest(selected, groups, share(0.25), nil)
		assert.Equal(t, []string{"a.md", "b.md"}, names(kept))
		assert.Empty(t, dropped)
	})

	t.Run("not json at all", func(t *testing.T) {
		_, err := parseGrouping("I grouped them by subject for you!", 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not return usable JSON")
	})

	t.Run("a reply that is not a partition is an ERROR, never a partial grouping", func(t *testing.T) {
		// 🚨 The whole safety property. Item 1 is missing, and the tempting
		// reading — "group what came back" — would drop it from the digest with
		// no absence recorded.
		_, err := parseGrouping(`{"subjects": [{"subject": "a", "items": [0, 2]}]}`, 3)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not partition the 3 selected items")
		assert.Contains(t, err.Error(), "item 1 was left out")
	})

	t.Run("an empty selected set can never be partitioned", func(t *testing.T) {
		// Which is why the caller skips the pass entirely on a quiet day rather
		// than calling it and reading the failure as a fault.
		_, err := parseGrouping(`{"subjects": []}`, 0)
		require.Error(t, err)
	})

	t.Run("a duplicate names which subjects disagree", func(t *testing.T) {
		_, err := parseGrouping(`{"subjects": [{"subject": "a", "items": [0, 1]}, {"subject": "b", "items": [1]}]}`, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"a" and "b"`)
	})

	t.Run("a duplicate inside one subject says so", func(t *testing.T) {
		_, err := parseGrouping(`{"subjects": [{"subject": "a", "items": [0, 0, 1]}]}`, 2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `appears twice in subject "a"`)
	})
}

// --- the balance pass, end to end ---

// balanceFixture builds a day of n items across two sources, for an edition
// configured with a subject share.
func balanceFixture(t *testing.T, day time.Time, n int, extraEditionYAML string) (*config.Config, string) {
	t.Helper()
	items := make([]store.Item, 0, n)
	for i := 0; i < n; i++ {
		source := "a-source"
		if i%2 == 1 {
			source = "b-source"
		}
		items = append(items, store.Item{
			Source:  source,
			URL:     "https://example.com/" + strconv.Itoa(i),
			Title:   "Item " + strconv.Itoa(i),
			Content: "body " + strconv.Itoa(i),
		})
	}
	return fixture(t, day, "profile", "editions:\n  "+config.DefaultEdition+":\n    "+extraEditionYAML+"\n", items...)
}

// TestTheCeilingTrimsTheDigestAndNamesEveryItemItRemoved is the pass's whole
// contract in one run: fewer items in the digest, and every missing one
// accounted for in the report under its own reason.
func TestTheCeilingTrimsTheDigestAndNamesEveryItemItRemoved(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := balanceFixture(t, day, 5, "subject_share: 0.25")

	client := &stubClient{
		group: func(req llm.Request) (llm.Response, error) {
			// Four items on one subject, one on another. The allowance is
			// max(1, floor(0.25*5)) = 1, so the crowded subject keeps one.
			return llm.Response{Content: splitSubjectsJSON(itemsAskedAbout(req), 1)}, nil
		},
	}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	require.Len(t, result.Editions, 1)
	edition := result.Editions[0]
	assert.Equal(t, 2, edition.Selected, "one item per subject survives")
	assert.Equal(t, 3, edition.Dropped)
	assert.False(t, edition.Empty)
	assert.False(t, edition.BalanceFailed)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Len(t, digest.Items, 2, "the digest carries what survived, not what was selected")
	assert.Empty(t, digest.BalanceFailed)

	_, report := readReport(t, root, day)
	require.Len(t, report.Editions, 1)
	assert.Len(t, report.Editions[0].Selected, 2)
	assert.Equal(t, 3, countAbsent(report.Editions[0].Absent, ReasonSubjectShare))

	// 🚨 The property the reason exists for: every item is accounted for exactly
	// once. An item both listed as selected and recorded as absent would make
	// the report's two lists disagree about the same day.
	inDigest := map[string]bool{}
	for _, name := range report.Editions[0].Selected {
		inDigest[name] = true
	}
	for _, absence := range report.Editions[0].Absent {
		assert.False(t, inDigest[absence.Item],
			"an item recorded as absent must not also be listed as selected")
	}
	assert.Equal(t, 5, len(report.Editions[0].Selected)+len(report.Editions[0].Absent))
}

// TestAnEditionWithNoShareRunsNoGroupingCall keeps the setting's absence free:
// an edition that has never heard of it behaves as it did before it existed,
// and pays for nothing.
func TestAnEditionWithNoShareRunsNoGroupingCall(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := fixture(t, day, "profile", "",
		store.Item{Source: "a-source", URL: "https://example.com/a", Title: "A", Content: "body"},
		store.Item{Source: "b-source", URL: "https://example.com/b", Title: "B", Content: "body"})

	client := &stubClient{}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Empty(t, client.callsIn("group"), "no share configured means no call is made")
	assert.Equal(t, 2, result.Editions[0].Selected)
	assert.Zero(t, result.Editions[0].Dropped)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Len(t, digest.Items, 2)
	assert.Empty(t, digest.BalanceFailed, "a pass that never ran is not a pass that failed")
}

// TestAQuietDayIsNotAGroupingFailure guards the one input the grouping pass
// cannot answer. An empty selected set has no valid partition — every reply
// fails validation — so calling the pass anyway would label a legitimately
// quiet day as a failure, in an engine whose stated position is that an empty
// digest is a correct outcome.
func TestAQuietDayIsNotAGroupingFailure(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := balanceFixture(t, day, 2, "subject_share: 0.25")

	client := &stubClient{
		sel: func(llm.Request) (llm.Response, error) {
			return llm.Response{Content: selectionJSON(false, 0.1)}, nil
		},
	}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Empty(t, client.callsIn("group"), "nothing was selected, so there is nothing to balance")
	assert.True(t, result.Editions[0].Empty)
	assert.False(t, result.Editions[0].BalanceFailed)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Contains(t, markdown, emptyDigestMarker)
	assert.Empty(t, digest.BalanceFailed)
	assert.NotContains(t, markdown, "could not be sorted by subject")
}

// TestAFailedGroupingWritesTheDigestUnbalancedAndSaysSo is ADR-0007 §8.
//
// ⚠️ The assertion that matters most is the last one. A full digest is exactly
// what a WORKING ceiling produces on a balanced day, so without the recorded
// cause the two days are the same document — and the one where the reader's own
// instruction went unenforced is the one they would want to know about.
func TestAFailedGroupingWritesTheDigestUnbalancedAndSaysSo(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := balanceFixture(t, day, 4, "subject_share: 0.25")

	client := &stubClient{
		group: func(llm.Request) (llm.Response, error) {
			return llm.Response{Content: "sorry, I could not do that"}, nil
		},
	}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err, "a failed ceiling costs the balance, never the digest")

	edition := result.Editions[0]
	assert.True(t, edition.BalanceFailed)
	assert.Equal(t, 4, edition.Selected, "every selected item is carried")
	assert.Zero(t, edition.Dropped)
	assert.False(t, edition.Empty)

	markdown, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Len(t, digest.Items, 4)
	assert.Contains(t, digest.BalanceFailed, "did not return usable JSON",
		"the cause is on the structured half, where tooling reads it")
	assert.Contains(t, markdown, "no subject was held to its usual share",
		"and the outcome is in the markdown, where the reader is")
	assert.NotContains(t, markdown, "usable JSON", "the error text never reaches the reader")

	_, report := readReport(t, root, day)
	assert.Zero(t, countAbsent(report.Editions[0].Absent, ReasonSubjectShare),
		"a ceiling that never ran removed nothing, so it recorded no absences")
}

// TestAnUnusableGroupingIsAskedAgainOnce mirrors the title and select passes:
// one retry on a reply that did not parse, because the likeliest failure of a
// hard-checked reply is a near-miss the same request would not repeat.
func TestAnUnusableGroupingIsAskedAgainOnce(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := balanceFixture(t, day, 4, "subject_share: 0.25")

	attempts := 0
	client := &stubClient{
		group: func(req llm.Request) (llm.Response, error) {
			attempts++
			if attempts == 1 {
				// A partition missing one index: the near-miss the retry is for.
				return llm.Response{Content: `{"subjects": [{"subject": "a", "items": [0, 1, 2]}]}`}, nil
			}
			return llm.Response{Content: splitSubjectsJSON(itemsAskedAbout(req), 1)}, nil
		},
	}
	result, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	assert.Equal(t, 2, attempts)
	assert.False(t, result.Editions[0].BalanceFailed)
	assert.Equal(t, 2, result.Editions[0].Selected)

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Empty(t, digest.BalanceFailed)
}

// TestTitlesAreOnlyPaidForWhatSurvives pins the pass's position in the sequence.
// Running it after the title pass would translate headlines for items about to
// be dropped — correct output, paid for twice over.
func TestTitlesAreOnlyPaidForWhatSurvives(t *testing.T) {
	day := at(t, "2026-09-04T06:00:00Z")
	cfg, root := balanceFixture(t, day, 5, "subject_share: 0.25\n    language: Persian")

	client := &stubClient{
		group: func(req llm.Request) (llm.Response, error) {
			return llm.Response{Content: splitSubjectsJSON(itemsAskedAbout(req), 1)}, nil
		},
	}
	_, err := runner(t, cfg, client, day).Run(context.Background(), day)
	require.NoError(t, err)

	titleCalls := client.callsIn("title")
	require.Len(t, titleCalls, 1)
	headlines := titleCalls[0].Messages[1].Content
	assert.Equal(t, 2, strings.Count(headlines, "\n0. ")+strings.Count(headlines, "\n1. ")+
		strings.Count(headlines, "\n2. ")+strings.Count(headlines, "\n3. ")+strings.Count(headlines, "\n4. "),
		"the title pass is given the survivors, not everything that was selected")

	_, digest := readDigest(t, root, day, config.DefaultEdition)
	assert.Len(t, digest.Items, 2)
}

// --- the digest length, and its interaction with the share (ADR-0008) ---

// TestTheShareIsMeasuredAgainstTheTargetNotTheSelectedSet is the property the
// whole decision turns on, and the fixture is built so the two rules give
// DIFFERENT answers rather than agreeing by luck.
//
// Twenty selected items across two subjects, a quarter, a target of eight:
//
//   - against the target (correct): allowance = floor(0.25 × 8) = 2, so each
//     subject keeps two and the digest is FOUR.
//   - against the selected set (the weaker rule): allowance = floor(0.25 × 20) = 5,
//     the fill runs to the target, and the digest is EIGHT.
//
// ⚠️ "No subject over about a quarter" was always a statement about the digest in
// front of the reader, not about an intermediate set he never sees. Measured
// against the selected set it is a far weaker rule than the one he asked for, and
// it was invisible for as long as the length lived in a different program.
func TestTheShareIsMeasuredAgainstTheTargetNotTheSelectedSet(t *testing.T) {
	var selected []selectedItem
	var a, b []int
	for i := 0; i < 20; i++ {
		selected = append(selected, item(fmt.Sprintf("i%02d.md", i), 1.0-float64(i)/100, 0.5))
		if i%2 == 0 {
			a = append(a, i)
		} else {
			b = append(b, i)
		}
	}
	groups := []subjectGroup{{Subject: "a", Members: a}, {Subject: "b", Members: b}}

	kept, dropped := fillDigest(selected, groups, share(0.25), entries(8))

	assert.Len(t, kept, 4, "two subjects at an allowance of two, not eight items at an allowance of five")
	assert.Equal(t, 2, shareAllowance(8, 0.25), "the allowance comes from the target")
	assert.Equal(t, 5, shareAllowance(len(selected), 0.25), "and would be this against the selected set")
	assert.Len(t, dropped, 16)
}

// TestATargetAloneShortensWithoutBalancing covers a length with no share: nothing
// bounds a subject except the length itself.
func TestATargetAloneShortensWithoutBalancing(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5), item("a3.md", 0.7, 0.5),
		item("a4.md", 0.6, 0.5), item("a5.md", 0.5, 0.5),
	}
	groups := []subjectGroup{{Subject: "one field", Members: []int{0, 1, 2, 3, 4}}}

	kept, dropped := fillDigest(selected, groups, nil, entries(3))

	assert.Equal(t, []string{"a1.md", "a2.md", "a3.md"}, names(kept),
		"a one-subject day is carried, shortened to the target")
	require.Len(t, dropped, 2)
	for _, d := range dropped {
		assert.False(t, d.LostToSubject, "nothing here lost its place to a subject's share")
	}
}

// TestTheFillNeverLetsOneSubjectTakeASlotAnotherIsWaitingFor is the round-robin
// property, and it is what removes the ordering trap.
//
// ⚠️ Sequencing would fail here in either direction. Take the strongest four first
// and they are all one subject; apply a share afterwards and the digest collapses.
// Balance first and then take the strongest four, and the length undoes the balance.
func TestTheFillNeverLetsOneSubjectTakeASlotAnotherIsWaitingFor(t *testing.T) {
	selected := []selectedItem{
		item("strong1.md", 0.99, 0.5), item("strong2.md", 0.98, 0.5),
		item("strong3.md", 0.97, 0.5), item("strong4.md", 0.96, 0.5),
		item("weak1.md", 0.10, 0.5), item("weak2.md", 0.09, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "the loud one", Members: []int{0, 1, 2, 3}},
		{Subject: "the quiet one", Members: []int{4, 5}},
	}

	kept, _ := fillDigest(selected, groups, nil, entries(4))

	// Two apiece, even though every item of the first subject outscores both of
	// the second's. Taking the strongest four would have given a single-subject
	// digest of exactly the kind the reader complained about.
	assert.Equal(t, []string{"strong1.md", "strong2.md", "weak1.md", "weak2.md"}, names(kept))
}

// TestTheFillComesUpShortRatherThanPadding is ADR-0008 §4, and the arithmetic
// yaad's review asked to have visible: the reachable length is
// min(target, subjects × allowance), so a day with few subjects cannot fill the
// target however much news it carries.
func TestTheFillComesUpShortRatherThanPadding(t *testing.T) {
	var selected []selectedItem
	var a, b, c []int
	for i := 0; i < 12; i++ {
		selected = append(selected, item(fmt.Sprintf("i%02d.md", i), 1.0-float64(i)/100, 0.5))
		switch i % 3 {
		case 0:
			a = append(a, i)
		case 1:
			b = append(b, i)
		default:
			c = append(c, i)
		}
	}
	groups := []subjectGroup{
		{Subject: "a", Members: a}, {Subject: "b", Members: b}, {Subject: "c", Members: c},
	}

	kept, _ := fillDigest(selected, groups, share(0.25), entries(8))

	// Three subjects × an allowance of two = six, short of the target of eight,
	// with four items still available. Nothing is taken to make up the difference.
	assert.Len(t, kept, 6)
	assert.Equal(t, 2, shareAllowance(8, 0.25))
}

// TestADroppedItemSaysWHICHLimitTookIt is ADR-0008 §6. The two reasons look
// identical to a reader of the digest and answer different questions: one is about
// the source list, the other about the configured length.
//
// Two subjects of three, a target of three, a share giving an allowance of two.
// The fill takes a1, b1, then a2 and stops on the length. So a3 was crowded out by
// its OWN subject reaching two, while b2 and b3 lost their place to the length with
// their subject still one short of its allowance.
func TestADroppedItemSaysWHICHLimitTookIt(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5), item("a3.md", 0.7, 0.5),
		item("b1.md", 0.6, 0.5), item("b2.md", 0.5, 0.5), item("b3.md", 0.4, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "a", Members: []int{0, 1, 2}},
		{Subject: "b", Members: []int{3, 4, 5}},
	}

	kept, dropped := fillDigest(selected, groups, share(0.7), entries(3))
	require.Equal(t, []string{"a1.md", "a2.md", "b1.md"}, names(kept))
	assert.Equal(t, 2, shareAllowance(3, 0.7))

	byName := map[string]droppedItem{}
	for _, d := range dropped {
		byName[d.Item.Item.Filename] = d
	}
	require.Contains(t, byName, "a3.md")
	assert.True(t, byName["a3.md"].LostToSubject, "a3 lost its place to its own subject's share")
	require.Contains(t, byName, "b2.md")
	assert.False(t, byName["b2.md"].LostToSubject,
		"b's subject never reached its allowance — the digest simply filled up, which is a different fact")
}

// TestTheFillWithNoTargetIsExactlyTheOldCeiling states the compatibility property
// as an assertion rather than leaving it to be inferred: an edition that configures
// a share and no length behaves exactly as it did under ADR-0007.
func TestTheFillWithNoTargetIsExactlyTheOldCeiling(t *testing.T) {
	selected := []selectedItem{
		item("a1.md", 0.9, 0.5), item("a2.md", 0.8, 0.5), item("a3.md", 0.7, 0.5),
		item("a4.md", 0.6, 0.5), item("a5.md", 0.5, 0.5), item("a6.md", 0.4, 0.5),
		item("b1.md", 0.3, 0.5), item("b2.md", 0.2, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "one field", Members: []int{0, 1, 2, 3, 4, 5}},
		{Subject: "another", Members: []int{6, 7}},
	}

	kept, dropped := fillDigest(selected, groups, share(0.25), nil)

	assert.Equal(t, []string{"a1.md", "a2.md", "b1.md", "b2.md"}, names(kept))
	assert.Len(t, dropped, 4)
}

// TestSubjectsAreFilledStrongestFirst pins the order the round robin visits
// subjects in, which decides who gets the last slot when the length runs out.
func TestSubjectsAreFilledStrongestFirst(t *testing.T) {
	selected := []selectedItem{
		item("weak.md", 0.10, 0.5),
		item("strong.md", 0.90, 0.5),
		item("middling.md", 0.50, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "third", Members: []int{0}},
		{Subject: "first", Members: []int{1}},
		{Subject: "second", Members: []int{2}},
	}

	kept, _ := fillDigest(selected, groups, nil, entries(2))
	assert.Equal(t, []string{"strong.md", "middling.md"}, names(kept),
		"the two strongest subjects get the slots, in that order")
}

// TestSubjectOrderFallsBackToTheLabel reaches the comparison that decides between
// two subjects whose strongest items are indistinguishable.
//
// ⚠️ It needs two items sharing a filename, which the pipeline never produces —
// item identity is unique within a day. That is the point: strongerMatch ends in a
// filename comparison, so through the real pipeline this branch is UNREACHABLE and
// the ordering is correct because of a property of a different function. This test
// exists so the branch is exercised by something, and so that weakening
// strongerMatch does not silently activate untested code.
func TestSubjectOrderFallsBackToTheLabel(t *testing.T) {
	selected := []selectedItem{
		item("same.md", 0.5, 0.5), item("same.md", 0.5, 0.5), item("same.md", 0.5, 0.5),
	}
	groups := []subjectGroup{
		{Subject: "charlie", Members: []int{0}},
		{Subject: "alpha", Members: []int{1}},
		{Subject: "bravo", Members: []int{2}},
	}

	kept, dropped := fillDigest(selected, groups, nil, entries(2))
	require.Len(t, kept, 2)
	require.Len(t, dropped, 1)
	assert.Equal(t, "charlie", dropped[0].Subject,
		"with nothing to choose between the items, the labels order the subjects")
}
