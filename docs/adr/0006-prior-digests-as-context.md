# ADR-0006: Prior digests are context for the writer, never candidates for selection

**Status:** Proposed

## Context

The engine has no memory across days. `buildSelectMessages` sees one enriched
item and the relevance profile; the writing pass sees the day's selected items
and the profile. Nothing anywhere reads yesterday's output.

So a running story is re-sent in full every morning it produces an article. The
reader's objection is not that the story is uninteresting, it is that he already
read it: *"if you send the same topic yesterday, today, tomorrow, day after,
what is the point?"*

He also drew the boundary himself, before being asked, and it is the reason this
needs a decision rather than a patch:

> *"maybe it's a good idea to keep the news of the last five digests in the
> context... I don't want them to use them as a source again. I want them to use
> as a context. This is very important. Do not see them as a source."*

A prior digest entering the candidate pool would be self-amplifying: the engine
would select yesterday's summary, summarise it again, and hand tomorrow a
summary of a summary. The requirement is memory without recirculation.

Two facts about the pipeline shape the answer. **The selection call runs once per
candidate** — a hundred times on a normal day — so anything added to its input is
paid a hundred times. **The writing call runs once.** And selection currently has
no target size and no cap; `Selected` is an output, not a quota, so removing an
item frees nothing and nothing backfills.

## Decision

**1. History is an edition-level setting, not a global one.** Each edition
declares how many prior digests of *its own* it carries, defaulting to none.
What a reader has already been sent is a property of the edition that sent it:
an English edition and a Persian edition drawing on one source pool have
different histories, and a newsletter's readers have not read the personal
brief.

**2. An item whose STABLE IDENTITY appears in the edition's last N digests is
dropped before selection, deterministically and with no model call.** Whether an
item was already sent is a fact, not a judgement, and paying a model to
re-derive a fact once per candidate is the arithmetic that makes a pipeline
expensive.

🔑 **The key is the item identity ADR-0002 already defines, not the URL.**
`Collected.URL` is documented as the item's address *when it has one*: the inbox
collector never sets it and a plugin may omit it. **A URL-keyed filter would be a
no-op rather than a weak check for exactly those items**, so they would re-send
every day — the complaint this decision exists to answer, surviving inside the
mechanism meant to fix it. `ItemKey` already resolves this: it digests the URL
when there is one and the content otherwise, and it is what the whole pipeline
already agrees an item *is*.

**`DigestItem` therefore gains the item key**, because a prior digest cannot
currently yield one: it carries source, url, title, score, reason, points, tags
and category, and none of those reconstructs an identity for an item with no
URL — **precisely the items this key was chosen to cover.** The field is
additive and a sink that does not know it ignores it.

🔑 **The digest is the right artifact to carry it, and the repo has already made
this exact argument once.** `Digest.Edition` exists because *"a sink and any
tooling downstream receive the document rather than its location"*. **An item
that cannot say which item it is has the same defect as a digest that cannot say
which edition it is.**

⛔ **Rejected: reading history from `ReportEdition.Selected`**, which already
stores `<source>--<key>.md` and would need no schema change. **It would make
suppression depend on report retention**, and reports are retained for evidence
rather than for function. A later decision to prune them would silently break
deduplication, with nothing connecting the two settings. **A functional
dependency on a telemetry artifact is a coupling that only shows up when someone
tidies up.**

⚠️ **Two limits, and they are different in kind. Both are stated here because
each looks like a bug from the outside:**

- **Semantic.** A second outlet reporting the same story is a *different item*
  and passes through untouched. "Already sent" sounds like a claim about stories;
  this filter makes a claim about items.
- **Structural.** For a content-keyed item, any shift in the text produces a
  different identity, so a republication with an edited word is not caught.

**Neither is a defect to be tuned away.** Widening this filter to catch either
one would mean guessing, and a wrong drop deletes a story the reader never learns
existed. §3 is where sameness is judged.

🔑 **The generalisable half, learned the expensive way one layer up: a cheap
exact test and an expensive semantic one answer different questions.** A lexical
title-similarity pass over four articles covering one event dropped **zero** of
them, because two headlines about one occurrence can share no vocabulary. The
cheap test is safe only while everyone knows which question it answered.

**3. The same story arriving by another route is the writing pass's problem, and
it gets condensed history to solve it** — for each of the last N digests, the
entries it contained as **`Title` plus the first entry of `Points`**, never the
full text. It is stated as what the reader has already been told, so a genuine
development is written as a development and an item with nothing new is dropped.

⚠️ **The fields are named because "a title and one line" does not identify one.**
`DigestItem` carries `Title`, `Points` and `Reason`, and only `Points` holds
extracted content.

🚫 **`Reason` must not be used, and this is a correctness rule rather than a
preference.** It is the justification for why an item matched *this edition's*
profile. ADR-0005 keeps per-audience reasoning out of shared artifacts precisely
so a private profile's judgement cannot surface in a public one; **carrying
`Reason` in a history block would reintroduce that leak through a new door**,
with the additional flaw that it describes why an item was picked rather than
what it said.

**Suppression at this layer must be visible.** An item selected and then written
away is invisible in a report that counts selections, which is the same seam
already documented on `ReportEdition`. So an entry the writer drops as
already-told is recorded, not silently absent.

**4. BOTH drop sites are counted and attributed in the day's report, under
distinct reasons.** The engine's existing posture is to prefer the failure that
leaves evidence behind. An item removed for a reason nobody can see is
indistinguishable from an item no source ever produced, and those two want
opposite responses.

There are two of them and they are not the same event:

| Where | What it means | Recorded as |
|---|---|---|
| Pre-selection (§2) | This exact link was already sent. | already sent, same item |
| Writing pass (§3) | This story was already told and nothing has moved. | already told, nothing new |

⚠️ **Collapsing them into one reason would destroy the signal the report exists
for.** A rising count of the first says a feed is republishing; a rising count of
the second says a story is being followed past the point where it develops. **The
same number for both answers neither question**, and the ADR's own principle —
that an invisible removal is indistinguishable from an item that never existed —
applies just as much to two removals that are indistinguishable from each other.

**5. History enters through the prompt and never through the candidate pool.**
Candidates come from the day's collected items, so a prior digest cannot become a
candidate unless someone deliberately configures it as a source.

🔑 **This is what makes his boundary hold by construction rather than by
discipline**, and it is the single sentence a future change must not break: the
moment history reaches selection as an item rather than as context, "context, not
sources" becomes a convention someone has to remember instead of a property of
the shape.

## Consequences

- Selection gains a pre-filter step and the writing pass gains an input. **No new
  pipeline stage**, so the sequence ADR-0005 fixes is unchanged.
- The enrichment cache is untouched. Enrichment stays neutral and keyed per item;
  history is applied after it and is not cached.
- The engine now depends on its own prior output being on disk. An edition
  configured for five days of history and holding two produces two days of
  history and says so, rather than failing.
- A reader adding an edition gets no history for it until it has run, which is
  correct and needs no special case.
- The condensed history is paid once per run rather than once per candidate. Full
  prior digests in the selection call would have been the expensive design, and
  it is rejected here for that reason rather than on principle.

## Not decided here

- **Grouping several articles about one event into a single unit before
  selection**, so the pipeline counts events rather than articles. That inserts a
  stage and the grouping is a property of a *set*, which the per-item enrichment
  cache cannot express. It is its own decision, and the seam it would close is
  documented on `ReportEdition`.
- **How long digests are retained.** History depends on retention, and retention
  is configured separately; this decision assumes the digests it asks for exist
  and reports honestly when they do not.
