# ADR-0007: A balance pass holds the selected set, so a subject's share is enforced where it can be recorded

**Status:** Proposed

## Context

The reader's instruction, after a digest came back almost entirely one subject:

> *"it is either all security items or all board game items, there is no
> in-between... I'd rather have like 20% to 25%"*

No layer can honour it today, and the reason is structural rather than a matter
of wording.

**Selection runs once per candidate.** `buildSelectMessages` is given one
enriched item and the profile: no digest, no other candidates, and no memory of
what it has already chosen. **A constraint on a distribution cannot be honoured
by a decision made per element.** The rule was written into the relevance
profile first and was a no-op — expressible there, and unenforceable.

**A per-source cap does not deliver it either**, and this was measured rather
than reasoned: a two-items-per-source ceiling still produced a digest five
eighths one subject, because three of the five live sources covered that
subject. Balance is *perceived* by subject and would be *enforced* by source, and
those are different partitions. A quota is a balance control only when its
buckets are the partition the reader actually perceives.

Which leaves the two stages that do hold a set:

| Stage | Sees | Emits |
|---|---|---|
| `admittedBySources(enriched, edition)` | the whole candidate set | a filtered set, **pre-selection** |
| `buildDigestMessages(profile, language, selected)` | the whole selected set | **prose only** |

The first holds every candidate but runs before selection, so it can cap what
*competes*, never what is *emitted*. The second holds exactly the right set and
can only express a decision as sentences.

### Why the writing pass is not the answer, although it can hold a set rule

One-event-one-entry is a set-level rule living in `digestSystemPrompt`, and it
works. So the objection is not that a prompt at that layer cannot bind.

**The objection is what the two rules do to an item.** A merge preserves every
fact: four articles about one breach become one entry carrying the fullest set of
facts between them, and its cost is a counting mismatch already documented on
`ReportEdition` — the report counts articles, the digest is written in events,
and both are correct about different things.

**A share ceiling discards items.** An entry dropped in the writing pass is
counted in `ReportEdition.Selected`, appears nowhere in the digest, and carries
no absence reason, because absences are computed before the writing pass and that
pass has no way to add one.

⚠️ **That is this project's signature failure, newly manufactured: an item that
is absent for a reason nobody recorded.** The absence reasons exist precisely
because an invisible removal is indistinguishable from an item no source ever
produced, and those two want opposite responses. Buying a visible improvement by
putting a hole in the accounting is the wrong trade, and it is the wrong trade
independently of how badly the visible improvement is wanted.

### A deterministic stage does not escape the problem, it relocates it

`Enrichment` carries `Category` and `Tags`, and **neither is the subject**:

- `category` is defined in `enrichSystemPrompt` as *the kind of thing this is* —
  "announcement, analysis, release, incident, interview, opinion, listing". It
  groups a security announcement with a board-game announcement, and splits one
  subject across release, opinion and listing.
- `tags` are "short lower-case topic labels", free-form and multi-valued, so an
  item belongs to several subjects at once and a share has no denominator.

⚠️ **A ceiling counted over either would be a rule that is always satisfied and
never binding** — with free-form per-item buckets almost nothing ever exceeds a
quarter — which is the same no-op as putting it in the profile, and harder to
notice, because permanent compliance looks exactly like success.

So the subject partition needs judgement. What it does not need is for the
*dropping* to need judgement too.

## Decision

**1. A balance pass runs between selection and writing.** It receives the items
one edition selected, and returns a subset of them plus an absence for each item
it removed. The writing pass's input becomes that subset; nothing else about it
changes.

This is a new stage in the sequence ADR-0005 fixes, which is why this is a
decision and not an implementation.

**2. Grouping is one model call over the whole selected set; the ceiling is
arithmetic over the grouping it returns.**

The split is the point. **Judgement where judgement is needed, bookkeeping where
bookkeeping is needed.** A model is asked only "which of these belong to the same
subject", answered with the set in front of it, the same way the writing pass is
asked to decide sameness from what the data points describe rather than from the
wording. Everything after that — which items exceed an allowance, which ones go,
what the report says about them — is computed.

⛔ **Rejected: one call that both groups and drops.** Its answer is
unauditable: a subject the model quietly merged and an item it quietly dropped
arrive as the same thing, a shorter list. Nothing downstream can tell them apart,
and neither can a test.

⛔ **Rejected: a per-item `subject` field on the neutral enrichment.** Subject is
audience-agnostic, so this would be legal under ADR-0005 §1 and cacheable, which
makes it the tempting design. **It does not work, for the reason the whole issue
turns on: labels assigned to items independently do not compose into a
partition.** Two board-game items read one at a time come back "board games" and
"tabletop", and a share counted over the result is the free-text no-op above
wearing a better name. A partition is a property of the set and has to be
produced by something holding the set.

⛔ **Rejected: a fixed subject vocabulary in config.** It makes the reader
enumerate as buckets what his profile already says in prose, and it mis-buckets
by construction: the day's genuinely new subject is not on the list, lands in
whatever "other" exists, and is capped together with everything else that did not
fit.

**3. The allowance is computed once, from the size of the set before any
dropping.** For a selected set of N items and a configured share `s`, each
subject may keep `max(1, floor(s × N))` items.

🔑 **Computed once is load-bearing, not an optimisation.** Recomputing the
allowance against the surviving set does not converge: every drop shrinks N,
which lowers the allowance, which forces another drop. **A naive
`while share > cap: drop the weakest` empties the digest** rather than balancing
it, and it does so silently, on exactly the days the ceiling was written for.

`max(1, …)` is the other half: **a subject that had news is never erased**, only
trimmed. A ceiling that can delete a subject outright is a worse version of the
complaint it answers.

⚠️ **The share is the allowance's denominator, not a guarantee about the
finished digest.** With two subjects present the result is nearer half and half
than a quarter each, and that is correct: the alternative is deleting facts to
reach a ratio no arrangement of two subjects can reach. **A later change that
iterates to make the achieved share match the configured one is the
non-terminating version above, and this paragraph is why it must not be made.**

**4. A single-subject day drops nothing.** When the grouping returns one subject,
the pass returns its input unchanged.

Not an exception bolted on: with one subject the share is 1.0 however many items
are kept, so dropping changes no proportion and only costs the reader facts. The
ceiling exists to stop one subject crowding the others out; where there are no
others, nothing is being crowded out.

**5. Within an over-allowance subject, the survivors are the highest-scoring
items, ties broken by salience and then by item key.** `Selection.Score` is how
strongly the item matched this edition's profile, which is the right question
here — the ceiling is deciding which of one subject's items this reader most
wanted, not which is most important in general. The tie-breaks exist so that a
re-run of a day produces the same digest; a balance pass that reshuffled on
re-run would make the engine's resume behaviour observable to the reader.

**6. Every dropped item is recorded as an absence, under its own reason.**

A new reason joins the four in `report.go`, and the doctrine there applies
unchanged: it must never be folded into another one. "Not selected by this
edition's profile" says the profile was applied and said no; this new reason says
the profile said yes and the item lost its place to others on the same subject.
**A rising count of the second says the source list is lopsided for this
reader — a diagnosis nothing else in the report offers**, and it is destroyed the
moment the two share a string.

**7. The pass shortens the digest and never lengthens it.** It removes items and
adds none. It cannot reach for a further item from a subject already at its
allowance to fill out a length, because it has no notion of a target length and
gains none here.

This keeps the engine's existing and correct position that a short or empty
digest is a valid outcome. Selection has no quota — `Selected` is an output, and
removing an item frees nothing and nothing backfills. **This decision introduces
the pipeline's first cap, and confines it to removal for that reason.**

**8. A failed grouping call writes the digest unbalanced and says so.** The
edition keeps every selected item and the ceiling does not run.

The engine's posture is that a failure costs as little as it can — a failed
selection costs one item, not the edition. Here the cheapest correct failure is
the previous behaviour: an unbalanced digest is what the reader gets today, and
an absent digest is a regression. ⚠️ **But an unbalanced digest with nothing
saying why is the invisible absence this ADR exists to prevent, arriving through
the back door**, so the run records that the balance pass did not run, and the
reader's own instruction is not silently unenforced.

## Consequences

- One additional model call per edition per run, over the selected set. It is the
  same order as the writing call rather than the per-candidate call, which is the
  arithmetic that keeps a pipeline affordable. It is **not cached**: the set is
  per edition and per day, so there is nothing a later run or another edition
  could reuse.
- The seam documented on `ReportEdition` narrows rather than widens.
  `Selected` still counts articles while the digest is written in events, but the
  items this pass removes now have a reason against them instead of vanishing.
- **What is covered by tests and what is not, stated plainly rather than
  implied.** The allowance arithmetic, the single-subject case, the
  `max(1, …)` floor, the tie-break ordering, the absence records and the
  failed-call path are ordinary deterministic code and are testable as such —
  which is the practical gain over a prompt rule, and it is a gain in
  *verifiability*, not only in accounting. **The grouping itself is a model
  judgement and is not covered.** A prompt assertion can show the instruction is
  present; it cannot show a digest came out balanced, and this ADR does not claim
  otherwise.
- ⚠️ **A balanced digest is not evidence that this works.** The balance currently
  reaching the reader is imposed by a per-subject ceiling in a downstream
  delivery script, and that script balances by accident rather than by design.
  Until delivery moves to the built-in sink, the informative artifact is the
  digest's own item distribution, never the delivered reading. **Reading a good
  delivered result as "the engine is fine now" would close this on evidence that
  says nothing about the engine.**
- If ADR-0006 §3 is accepted, the two removals compose: this pass runs first, and
  the writer may then drop an already-told entry, so a subject can finish below
  its allowance. That is not a defect, and the two removals keep distinct reasons.
- The configured share is one number per edition. An edition that does not set it
  runs no balance pass and pays for no grouping call, so the behaviour of an
  existing configuration is unchanged.

## Not decided here

- **The default share, and whether editions differ.** The reader said 20% to 25%
  about his own digest. Whether that is the shipped default is a question for the
  configuration, not for the mechanism, and the mechanism is indifferent to the
  number.
- **Grouping several articles about one event before selection**, so the pipeline
  counts events rather than articles end to end. This pass groups by subject,
  which is coarser, and it runs after selection, so it closes none of that. It
  remains its own decision.
- **Moving delivery to the built-in sink.** It is sequenced behind this: subject
  share has to be a property of the written digest before the downstream script
  that currently imposes it can be retired.
