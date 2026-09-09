package aggregate

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// parseGrouping reads the grouping pass's reply and returns it only if it is a
// genuine partition of the n items the pass was given.
//
// 🚨 The validation is not defensive tidiness. A reply that left an item out
// would remove it from the digest with no absence recorded, which is the exact
// failure ADR-0007 exists to prevent, arriving inside the mechanism built to
// prevent it. Rejecting the whole reply is the safe direction: an unbalanced
// digest is what the reader gets today, while a silently shortened one is worse
// than either.
func parseGrouping(content string, n int) ([]subjectGroup, error) {
	text := unfence(content)

	var reply struct {
		Subjects []struct {
			Subject string `json:"subject"`
			Items   []int  `json:"items"`
		} `json:"subjects"`
	}
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		return nil, fmt.Errorf("grouping pass did not return usable JSON: %w (got: %s)", err, snippet(text))
	}

	groups := make([]subjectGroup, 0, len(reply.Subjects))
	for i, s := range reply.Subjects {
		subject := strings.TrimSpace(s.Subject)
		if subject == "" {
			// A cluster with no name is still a cluster, and refusing the whole
			// reply over a missing label would cost a correct partition for a
			// cosmetic reason.
			//
			// 🚨 The placeholder is NUMBERED, and that is load-bearing rather
			// than cosmetic. mergeSameSubject folds entries sharing a label, so
			// giving every unlabelled cluster the same placeholder would merge
			// clusters the pass never said were related — and merging subjects
			// that are not one subject DROPS items, which is the expensive
			// direction. Distinct placeholders keep them distinct.
			subject = fmt.Sprintf("unnamed %d", i+1)
		}
		groups = append(groups, subjectGroup{Subject: subject, Members: s.Items})
	}

	if err := validateGrouping(groups, n); err != nil {
		return nil, fmt.Errorf("grouping pass did not partition the %d selected items: %w", n, err)
	}
	return groups, nil
}

// subjectGroup is one subject and the selected items that belong to it, named
// by their positions in the edition's selected slice.
//
// Positions rather than filenames because the grouping is produced by a model
// pass over a numbered list, and a reply that names positions can be checked
// against what was asked. A reply naming filenames would have to be matched by
// string, where a near-miss reads as an item the grouping simply omitted —
// which is the one failure this pass must not turn into a silent drop.
type subjectGroup struct {
	// Subject is what the grouping pass called this cluster. It is carried for
	// the report and the logs and is never matched on: nothing downstream
	// compares two subjects for equality, because two runs are free to name the
	// same cluster differently and that is not an error.
	Subject string

	// Members are indices into the selected slice, in no required order.
	Members []int
}

// droppedItem is one item the ceiling removed, kept alongside the subject it
// lost its place to so the report can say more than that it is gone.
type droppedItem struct {
	Item    selectedItem
	Subject string
}

// validateGrouping checks that a grouping is a PARTITION of n selected items:
// every index in range, every index exactly once, no empty groups.
//
// 🚨 This is the pass's safety property, not a tidiness check. The ceiling's
// whole purpose is that a removed item is accounted for, so a grouping that
// quietly omitted an index would delete that item from the digest with nothing
// recorded — manufacturing the exact failure ADR-0007 exists to prevent, inside
// the mechanism meant to prevent it. A grouping that fails here is treated as a
// failed grouping pass (ADR-0007 §8): the digest is written unbalanced and the
// run says so.
//
// Duplicates are rejected for the mirror reason: an index in two groups would be
// counted against two allowances and could be kept by one while being dropped by
// the other, so the same item is both present and recorded as absent.
func validateGrouping(groups []subjectGroup, n int) error {
	if len(groups) == 0 {
		return fmt.Errorf("the grouping named no subjects for %d selected items", n)
	}

	seen := make([]bool, n)
	dup := make([]string, n)
	counted := 0
	for _, group := range groups {
		if len(group.Members) == 0 {
			return fmt.Errorf("subject %q holds no items", group.Subject)
		}
		for _, index := range group.Members {
			if index < 0 || index >= n {
				return fmt.Errorf("subject %q names item %d, which is outside the %d selected", group.Subject, index, n)
			}
			if seen[index] {
				// Named separately because the two say different things about
				// the reply: twice inside one subject is a careless list, twice
				// across subjects is a genuine contradiction about where the
				// item belongs. Both are rejected; only one of them is a
				// disagreement.
				if dup[index] == group.Subject {
					return fmt.Errorf("item %d appears twice in subject %q", index, group.Subject)
				}
				return fmt.Errorf("item %d appears in more than one subject: %q and %q", index, dup[index], group.Subject)
			}
			dup[index] = group.Subject
			seen[index] = true
			counted++
		}
	}

	if counted != n {
		// Naming the first missing index rather than the count: "27 of 31" sends
		// the reader to count the reply, and the index is what they would be
		// looking for.
		for i, ok := range seen {
			if !ok {
				return fmt.Errorf("item %d was left out of the grouping, which covers %d of %d selected items", i, counted, n)
			}
		}
	}
	return nil
}

// mergeSameSubject folds entries carrying the same label into one subject.
//
// ⚠️ Without it the ceiling silently under-binds. Two entries both labelled
// "board games" are two subjects, each drawing the full allowance, so a subject
// split across two entries keeps twice what it should — a four-item subject
// returned as two entries of two keeps four where it should keep two. The
// grouping is still a genuine partition, so validateGrouping cannot see it,
// nothing in the digest or the report says the ceiling did less than it was
// asked to, and the failure runs in the direction of the original complaint.
//
// 🔑 A rule that reports compliance without binding is exactly what ADR-0007
// exists to end — it is what the relevance profile's version of this rule turned
// out to be. Catching it here keeps the fix on the arithmetic side of the split,
// where it can be tested, rather than leaving it to the grouping pass, where it
// could not be.
//
// 🚨 An EMPTY label never merges with anything, including another empty one. A
// label is the only thing distinguishing these entries, so an unlabelled cluster
// asserts nothing about which subject it is; folding two of them together would
// combine subjects that may be unrelated, and THAT error drops items rather than
// keeping them. Between the two directions, under-binding costs balance and
// over-binding costs the reader facts, so the uncertain case fails towards
// keeping.
func mergeSameSubject(groups []subjectGroup) []subjectGroup {
	merged := make([]subjectGroup, 0, len(groups))
	at := make(map[string]int, len(groups))

	for _, group := range groups {
		// Copied rather than aliased: the merge appends to whichever entry
		// arrived first, and appending to the caller's slice could write into
		// the backing array of a grouping it still holds.
		members := append([]int(nil), group.Members...)

		if i, seen := at[group.Subject]; seen && group.Subject != "" {
			merged[i].Members = append(merged[i].Members, members...)
			continue
		}
		if group.Subject != "" {
			at[group.Subject] = len(merged)
		}
		merged = append(merged, subjectGroup{Subject: group.Subject, Members: members})
	}
	return merged
}

// shareAllowance is how many items one subject may keep, for a selected set of
// n items and a configured share.
//
// 🔑 It is computed ONCE, from n, and never from the surviving set. Recomputing
// as items are dropped does not converge: each drop shrinks the set, which
// lowers the allowance, which forces another drop. The naive loop empties the
// digest rather than balancing it, and it does so on exactly the days a ceiling
// is wanted for. ADR-0007 §3.
//
// The floor of one is the other half: a subject that had news is trimmed, never
// erased. A ceiling that can delete a subject outright is a worse version of the
// complaint it answers.
func shareAllowance(n int, share float64) int {
	allowance := int(math.Floor(share * float64(n)))
	if allowance < 1 {
		return 1
	}
	return allowance
}

// applyShareCeiling trims each subject to its allowance and returns what
// survives, in the order it was given, together with what it removed.
//
// The input order is preserved because it is the order the writing pass would
// otherwise have seen; reordering the digest as a side effect of balancing it
// would be a second, unasked-for change hidden inside this one.
//
// The caller is responsible for validating the grouping first. Passing an
// invalid one is a programming error rather than a runtime outcome, so this
// panics rather than silently doing something reasonable — see validateGrouping
// for why a partial grouping must never reach the arithmetic.
func applyShareCeiling(selected []selectedItem, groups []subjectGroup, share float64) (kept []selectedItem, dropped []droppedItem) {
	if err := validateGrouping(groups, len(selected)); err != nil {
		panic("applyShareCeiling: " + err.Error())
	}

	groups = mergeSameSubject(groups)

	// ⚠️ One subject means the ceiling cannot bind, and this is not an
	// optimisation guarding the loop below — it is a decision (ADR-0007 §4).
	// With a single subject the share is 1.0 however many items are kept, so
	// dropping changes no proportion and only costs the reader facts. The
	// ceiling exists to stop one subject crowding the others out; here there
	// are no others.
	if len(groups) == 1 {
		return selected, nil
	}

	allowance := shareAllowance(len(selected), share)

	survives := make([]bool, len(selected))
	subjectOf := make([]string, len(selected))
	for _, group := range groups {
		members := append([]int(nil), group.Members...)
		sort.SliceStable(members, func(a, b int) bool {
			return strongerMatch(selected[members[a]], selected[members[b]])
		})
		for rank, index := range members {
			subjectOf[index] = group.Subject
			survives[index] = rank < allowance
		}
	}

	for i, item := range selected {
		if survives[i] {
			kept = append(kept, item)
			continue
		}
		dropped = append(dropped, droppedItem{Item: item, Subject: subjectOf[i]})
	}
	return kept, dropped
}

// strongerMatch orders two items of one subject by which the ceiling should
// keep first.
//
// Score leads because the question inside an over-allowance subject is which of
// these this reader wanted, and Score is how strongly the item matched this
// edition's profile. Salience breaks a tie because between two equally matching
// items the more substantive one is the better use of the space it takes.
//
// 🔑 The filename break is not decoration. Without a total order the survivors
// depend on the order the sort happened to see, so re-running a day could
// produce a different digest from the same inputs — which would make every
// comparison between two runs meaningless, and would surface the engine's
// resume behaviour to the reader as churn.
func strongerMatch(a, b selectedItem) bool {
	if a.Selection.Score != b.Selection.Score {
		return a.Selection.Score > b.Selection.Score
	}
	if a.Enrichment.Salience != b.Enrichment.Salience {
		return a.Enrichment.Salience > b.Enrichment.Salience
	}
	return a.Item.Filename < b.Item.Filename
}
