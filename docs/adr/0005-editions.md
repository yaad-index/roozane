# ADR-0005: Editions, a neutral enrichment pass, and the daily report

**Status:** Proposed

## Context

Today the pipeline narrows to a single digest: the aggregator judges each item
against one relevance profile, writes `digests/<day>.{md,json}`, and every sink
delivers that same file. That is correct for one reader and wrong for the
product ADR-0001 describes, whose reusability law says these three layers
should run a public newsletter alongside a personal brief.

Three requirements arrived together, and they are one design because each
answers a problem the next one creates.

1. **Many audiences, with overlap.** *"pull as many sources, and then have
   sinks that selects a bunch of them to a different sink"*, and decisively:
   *"my boardgame newsletter can [be] a subset of board game items that I have
   in my personal feed."* The outputs are not a partition. The same item
   legitimately appears in a public newsletter and a private brief on one day.
2. **Judge once, select many times.** *"the first pass is going to summerize
   and tag and categories and the second pass will pick them for use in a
   sink."*
3. **A daily report, because the point is to tune it.** *"I get to tweak things
   and it needs a telemetry to read from and understand why this item is here
   and why this news is not."*

Overlap rules out the two cheap designs for (1): routing an item to a sink —
one item, one destination — cannot express an item in two outputs, and
filtering a shared digest per sink cannot produce two *voices*, since a
newsletter for strangers and a brief for one reader differ in what they
explain, not only in which items survive. So the narrowing happens before the
digest is written.

That alone would multiply cost and create a leak: with the relevance judgement
made per audience, an overlapping item is judged once per profile, and the
existing resume cache — keyed on item filename alone
(`state.Items[item.Filename]`) — would serve a private profile's reasoning into
a public digest. Requirement (2) dissolves both problems rather than mitigating
them, which is why it is adopted here rather than deferred: **if the per-item
pass carries no audience, there is no per-audience reasoning to duplicate or to
leak.**

## Decision

**Split aggregation into a neutral ENRICH pass and a per-edition SELECT pass,
and give every audience an edition.** Collection is untouched: one pool, one
copy of each item on disk.

```
collect ──▶ days/<day>/items/*.md          # unchanged, audience-agnostic
enrich  ──▶ days/<day>/state.json          # summary, tags, category, salience
select  ──▶ digests/<edition>/<day>.{md,json}   # once per edition
report  ──▶ reports/<day>.{md,json}            # after every edition
deliver ──▶ each sink, for the edition or the report it names
```

```yaml
editions:
  personal:                                  # no `sources:` = the whole pool
    profile: /etc/roozane/profile.md
  boardgames:
    sources: [bgg-blog, bgg-hotness, dicebreaker]
    profile: /etc/roozane/boardgames.md

sinks:
  morning:    {edition: personal,   type: telegram, params: {chat_id: "…"}}
  archive:    {edition: personal,   type: file,     params: {path: "…/{day}.md"}}
  newsletter: {edition: boardgames, command: [/usr/local/lib/roozane-plugins/mailer]}
  telemetry:  {report: true,        type: file,     params: {path: "…/report-{day}.md"}}
```

1. **ENRICH runs once per item, ever, and knows nothing about any audience.**
   It produces a summary, tags, a category and a **generic salience score** —
   "is this substantive at all", not "does this reader care" — and records them
   in `days/<day>/state.json`, which ADR-0002 §5 already assigns to the
   aggregator. **The resume cache therefore keeps its current key, the item
   filename**, because a neutral result is the same result for everybody. It is
   invalidated by the enrichment model or prompt version changing, which the
   record carries, not by anything about a reader.

2. **SELECT runs once per edition and is where taste lives.** An edition picks
   from the enriched pool using its **source list first, then its own relevance
   profile**, and writes its own digest in its own voice.
   **The two selection layers are asymmetric, and saying they are merely "both
   needed" invites a reader to find the counterexample and doubt the section.**
   The source list cannot be replaced by the profile's job — `sources` cannot
   express "only the boardgame items inside a general feed" — but the profile
   *could* do the source list's job in prose, just non-deterministically,
   ungreppably, and at a judging call per unrelated item. **The source list
   earns its place on determinism, greppability and cost, not expressiveness.**

3. **Selection: omitting `sources` selects the whole pool; `sources: []`
   selects nothing** (a legitimate way to park an edition without deleting it).
   There is deliberately no `all` keyword: source ids are `[a-z0-9-]`, so `all`
   is a legal source id and a magic token would collide with a real source the
   day someone names one. Absence cannot collide.

4. **Digests live at `digests/<edition-id>/<day>.{md,json}`**, edition ids
   constrained like source ids since they become path components. **With no
   `editions:` block the engine behaves as one edition named `default`** over
   all sources and the top-level `relevance_profile`, so a single-reader config
   keeps no ceremony and one audience is the degenerate case of the general
   rule rather than a second code path.

5. **A sink names exactly one edition** (`edition:`, default `default`); many
   sinks may name the same one, which is how one digest reaches both a chat and
   a file. An unknown edition fails at config load, unlike a sink `type`,
   because the edition list is closed and known locally.

6. **Collection outcomes are persisted for the day, as telemetry only.**
   `days/<day>/collected.json` records, per source, whether it ran, how many
   items it produced, and whether it failed with the error text.
   🚨 **This is required because the information genuinely does not exist
   today.** ADR-0004 §1 decides deliberately that a *failed* fetch writes no
   `ran/` marker, so that a failed source stays "not run" and is retried — the
   ambiguity between "not due" and "failed" is load-bearing for `due()`, not an
   oversight. Therefore `ran/` cannot answer "what failed", and the report
   below needs exactly that.
   ⚠️ **`due()` must never read this file.** It is written for humans and for
   the report; the moment scheduling consults it, ADR-0004's retry property
   silently changes meaning. The invariant is stated here because the file
   looks useful to `due()` and the damage would be invisible.

7. **Each day gets a report at `reports/<day>.{md,json}`, and a sink may name
   it exactly as it names an edition** (`edition: personal` or `report:
   true`). 🔑 **The report is NOT an edition, and forcing it into one was
   wrong.** It has no sources and no profile, it selects nothing by
   construction, and it must run *after* every edition because it describes
   their outcomes — so every rule editions carry would need an exception for
   it. **What actually needed widening is what a sink may be pointed at, not
   what an edition is.** That keeps ADR-0001's dumb-sink law intact — the
   engine still generates, the sink still only delivers — without bending
   edition semantics around a document that is not an audience.

   It states, per source, what ran and what it yielded or how it failed; per
   item, its tags, category, salience, and enrichment failures (`state.json`
   already records these as `StatusFailed` with the error); per edition, what
   it selected and why each enriched item was not; and **per pass and per
   model, prompt and completion tokens separately, plus wall time.**

   💶 **Money only if the config supplies prices, and priced per direction.**
   The engine is provider-agnostic by ADR-0001 §3 and cannot know what a model
   charges; a hardcoded table would be wrong and a de facto endorsement. Prices
   are configured **per model as separate input and output rates**, because
   `llm.Usage` already splits `PromptTokens` from `CompletionTokens` and every
   real provider charges them differently — a single blended figure would
   misprice every model and throw away data already collected.

   **Why an item is absent has five answers, and only one is unanswerable:**

   | Why | Recoverable? |
   |---|---|
   | No source covers the topic at all | **No — invisible by construction** |
   | A source that covers it failed today | Yes, from §6's outcomes |
   | Collected, but enrichment failed | Yes, `StatusFailed` in `state.json` |
   | Enriched, below the generic salience floor | Yes |
   | Enriched, not selected by this edition | Yes, with which reason |

   Only the first is invisible: the item never entered the pipeline and no
   reporting can describe what was never fetched. The report surfaces
   per-source yield so that gap is legible and the reader draws the conclusion.
   ⚠️ **An earlier draft folded the second row into the first and so claimed a
   failed covering source was unanswerable — which is exactly what §6 exists to
   fix.** Persisting outcomes is pointless if the report then files that case
   under "invisible".

8. **An edition that selects nothing, or whose items all fall below the bar,
   still writes an empty digest** (ADR-0002 §4), so a quiet edition proves it
   ran. It carries the collection outcome of the sources it selected, so a
   narrow edition cannot dilute a total upstream failure into something that
   reads exactly like a quiet day — which it otherwise would, far more easily
   than the whole-pool digest ever could.
   ⚠️ **The two digest files carry this differently, because ADR-0002 already
   splits them by audience.** The `.json` records outcomes structurally,
   error text included, for sinks and tooling. **The `.md` states only that a
   selected source produced nothing, never the raw error** — a public
   newsletter's markdown must not end with a fetch exception, and "the digest"
   without a file named is precisely how that ships.

## Consequences

- **This supersedes ADR-0002's closing answer to the same question.** That ADR
  said the public-newsletter case would be "a different data root with a
  different config… a second, fully independent pipeline". Editions replace
  that with one deployment over one pool, because two pipelines would collect
  every shared item twice and could never express an item belonging to both. A
  reader grepping for the newsletter case must not find the old answer
  unmarked.
- **Cost stops scaling with audiences.** The expensive per-item call happens
  once regardless of how many editions exist, so a new edition costs one
  selection pass over already-enriched items. The first draft of this ADR had
  it scale with distinct profiles; this is strictly cheaper and the reason the
  neutral pass was adopted.
- **A whole failure class disappears rather than being defended against.** With
  no audience in the per-item record, no private reasoning can reach a public
  digest, and the resume cache needs no profile dimension.
- **The price is where noise is suppressed.** A neutral pass cannot apply
  taste, so it can only drop obvious junk on generic salience; the real noise
  ceiling — the 142-of-207 suppression the live deployment performs today —
  moves into selection, where each edition's profile applies. Selection sees
  more candidates than judging did, and the enriched summary rather than the
  full item is what it reads.
- **Two schemas bump: `state.json` (now enrichment, not judgement) and the
  digest JSON.** The existing schema-mismatch path already starts a day fresh,
  so migration costs one re-enriched day rather than a converter.
- **Retention must learn the nested shape, and fails silently until it does.**
  `pruneDigests` skips directory entries outright (`if e.IsDir() { continue }`),
  so the moment digests move under `digests/<edition>/`, a configured
  `retention.digests` quietly becomes keep-forever — no error, just unbounded
  growth. **`reports/` is a sibling tree with the same problem and needs its
  own retention rule**, which is a reason to keep it a distinct path rather
  than hiding it under `digests/report/`. **Neither is a follow-up; both ship
  with the change that moves the paths.**
- **The digest path changes shape for everyone**, including the single-reader
  case: `digests/2026-09-04.md` becomes `digests/default/2026-09-04.md`. A
  one-time break taken deliberately rather than special-casing the default
  edition into the old path, because a layout with one rule stays greppable and
  a layout with an exception does not. Files already written stay where they
  are.
- **`config.example.yaml` has to move off `digests/`.** Its `local-file` sink
  writes `path: digests/{day}.md`, and the file sink resolves a path as given
  rather than against the data root, so run from the data root that example
  recreates a flat `digests/<day>.md` beside the new `digests/default/`,
  reintroducing by example the exact shape this ADR removes.
- **No name is reserved, which is the point of not making the report an
  edition.** A user may have an edition called `report`; a sink says either
  `edition: <id>` or `report: true`, and the two namespaces never meet. An
  earlier draft reserved the name and so reintroduced exactly the collision
  shape this ADR refuses for the `all` token.
- **Ordering becomes part of the contract: every edition is written before the
  report**, since the report describes what they selected. This is the only
  ordering the design imposes, and it exists because the report is downstream
  of editions rather than one of them.
- **The report is what makes the profile tunable, and it is the first artefact
  the engine writes for its owner rather than for a reader.** Whether tuning
  becomes interactive — reacting to items and having the engine learn — stays
  open; this ADR gives that work the data it would need and does not presume
  its shape.
