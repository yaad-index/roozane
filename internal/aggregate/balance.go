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
	for _, s := range reply.Subjects {
		// A cluster with no name is still a cluster, and refusing the whole reply
		// over a missing label would cost a correct partition for a cosmetic
		// reason. So it is kept, and kept UNLABELLED.
		//
		// 🚨 There is deliberately no placeholder. An earlier version wrote
		// "unnamed <n>" here, which put a sentinel into the same namespace as
		// real labels: a grouping that genuinely returned a subject called
		// "unnamed 2" would fold together with the second unlabelled cluster,
		// and merging subjects that are not one subject DROPS items. Numbering
		// fixed the collision between placeholders and left the one with real
		// values, which is the same bug an order of magnitude quieter.
		//
		// An empty string is not a sentinel in that namespace, it is the absence
		// of a label — and mergeSameSubject never folds on it. If the label is
		// ever wanted for display, it wants a separate field, so the distinction
		// stops being carried by the string's contents.
		groups = append(groups, subjectGroup{Subject: strings.TrimSpace(s.Subject), Members: s.Items})
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
	// Subject is what the grouping pass called this cluster, and it is EMPTY
	// when the pass named no subject.
	//
	// ⚠️ It IS matched on, within a single grouping: mergeSameSubject folds
	// entries carrying the same label. That is why the empty case must stay
	// empty rather than being filled with a placeholder — a placeholder is a
	// value in this namespace and can equal a real label. Across runs nothing
	// compares subjects, since two runs are free to name the same cluster
	// differently and that is not an error.
	Subject string

	// Members are indices into the selected slice, in no required order.
	Members []int
}

// droppedItem is one item the ceiling removed, kept alongside the subject it
// lost its place to so the report can say more than that it is gone.
type droppedItem struct {
	Item    selectedItem
	Subject string

	// LostToSubject says this item was crowded out by its OWN subject reaching its
	// allowance, rather than by the digest reaching its length.
	//
	// 🚨 The two must not be reported as one thing. A rising count of the first
	// says this reader's source list has tilted towards one subject — a statement
	// about the configuration. A rising count of the second says there was more
	// news than the configured length admits — a statement about the length. The
	// same reason for both answers neither question (ADR-0008 §6).
	LostToSubject bool
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
//
// 🔑 That guard is the LIVE path for every unlabelled cluster, not a contract
// for a hypothetical caller: parseGrouping leaves an unnamed subject empty and
// writes no placeholder, so this is what keeps two of them apart.
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

// fillDigest selects the entries a digest carries: at most `target` of them, with
// no subject taking more than its allowance, returning what survives in the order
// it was given together with what it left out.
//
// 🔑 A length and a share are ONE selection problem, not two filters (ADR-0008 §3).
// This fills subject by subject — strongest first within each subject, subjects in
// a stable order, one item at a time — so a subject never takes a slot while
// another is still waiting for one. Applying them in sequence is what creates the
// degenerate cases, and both orders have one:
//
//   - cap then ceiling: if the strongest N are all one subject, the ceiling cuts
//     them to the allowance and the digest is two entries.
//   - ceiling then cap: the cap can take N items from a single subject, undoing the
//     balance the previous step just imposed.
//
// Neither is visible from either rule alone, which is why the fill is a decision
// rather than an implementation detail.
//
// ⚠️ It fails towards SHORT. When every subject is at its allowance and the target
// is not met, the digest is shorter than the target and that is correct (ADR-0008
// §4) — the failure mode is the one the engine already treats as valid rather than
// the one the reader complained about.
//
// The input order is preserved because it is the order the writing pass would
// otherwise have seen; reordering the digest as a side effect of balancing it would
// be a second, unasked-for change hidden inside this one.
//
// target and share are both optional. With no target the digest is as long as the
// selected set, which reduces exactly to ADR-0007's ceiling; with no share a
// subject is bounded only by the target.
//
// The caller must validate the grouping first — see validateGrouping for why a
// partial grouping must never reach the arithmetic.
func fillDigest(selected []selectedItem, groups []subjectGroup, share *float64, target *int) (kept []selectedItem, dropped []droppedItem) {
	if err := validateGrouping(groups, len(selected)); err != nil {
		panic("fillDigest: " + err.Error())
	}

	groups = mergeSameSubject(groups)

	limit := len(selected)
	if target != nil && *target < limit {
		limit = *target
	}

	// ⚠️ One subject means the SHARE cannot bind, and this is a decision rather
	// than an optimisation (ADR-0007 §4): with a single subject the share is 1.0
	// however many items are kept, so applying it changes no proportion and only
	// costs the reader facts. A TARGET still applies — a one-subject day is
	// carried, shortened to the target — which is why this sets the allowance
	// rather than returning early.
	//
	// shareBinds is tracked rather than inferred from `allowance == limit`. Without
	// a share the allowance IS the limit, so a subject that fills the whole digest
	// would look like one that hit a share ceiling — and every item it crowded out
	// would be reported under the wrong reason, which is the one thing §6 exists to
	// prevent.
	allowance := limit
	shareBinds := share != nil && len(groups) > 1
	if shareBinds {
		allowance = shareAllowance(limit, *share)
	}

	// Subjects in a stable order, each with its members strongest-first. Both are
	// total orders, so re-running a day produces the same digest; a fill that
	// reshuffled between runs would make every comparison between two runs
	// meaningless (ADR-0008 §5).
	// ⚠️ The subject travels WITH its members. An earlier version sorted a slice
	// of member lists and reached back into `groups` by index for the label — but
	// the two only correspond until the sort's first swap, after which it compared
	// the names of unrelated subjects.
	//
	// 🔑 It never misbehaved, and that is the part worth recording: strongerMatch
	// ends in a filename comparison and filenames are unique, so one of the two
	// calls below always answers true and the label branch is unreachable. The
	// code was correct because of a property of a DIFFERENT function, and weakening
	// that one — dropping the filename fallback, or ordering on something that can
	// genuinely tie — would have activated the bug silently, since the output stays
	// deterministic and merely stops being the order intended.
	ordered := make([]subjectGroup, 0, len(groups))
	for _, group := range groups {
		members := append([]int(nil), group.Members...)
		sort.SliceStable(members, func(a, b int) bool {
			return strongerMatch(selected[members[a]], selected[members[b]])
		})
		ordered = append(ordered, subjectGroup{Subject: group.Subject, Members: members})
	}
	sort.SliceStable(ordered, func(a, b int) bool {
		first, second := ordered[a].Members[0], ordered[b].Members[0]
		if strongerMatch(selected[first], selected[second]) {
			return true
		}
		if strongerMatch(selected[second], selected[first]) {
			return false
		}
		return ordered[a].Subject < ordered[b].Subject
	})

	survives := make([]bool, len(selected))
	subjectOf := make([]string, len(selected))
	atAllowance := make([]bool, len(selected))
	taken := 0

	for _, group := range groups {
		for _, index := range group.Members {
			subjectOf[index] = group.Subject
		}
	}

	// Round robin: one item per subject per pass, so a subject cannot take a
	// second slot while another subject is still waiting for its first.
	for round := 0; taken < limit; round++ {
		progressed := false
		for _, group := range ordered {
			if taken >= limit {
				break
			}
			if round >= allowance || round >= len(group.Members) {
				continue
			}
			survives[group.Members[round]] = true
			taken++
			progressed = true
		}
		if !progressed {
			break
		}
	}

	// An item left out for its subject's share and one left out because the digest
	// was full are different facts, and the report must be able to tell them apart
	// (ADR-0008 §6). A subject that reached its allowance crowded out its own
	// remainder; anything else lost its place to the length.
	countBySubject := map[string]int{}
	for i := range selected {
		if survives[i] {
			countBySubject[subjectOf[i]]++
		}
	}
	for i := range selected {
		atAllowance[i] = shareBinds && countBySubject[subjectOf[i]] >= allowance
	}

	for i, item := range selected {
		if survives[i] {
			kept = append(kept, item)
			continue
		}
		dropped = append(dropped, droppedItem{
			Item:          item,
			Subject:       subjectOf[i],
			LostToSubject: atAllowance[i],
		})
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
