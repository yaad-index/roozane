# ADR-0005: Editions — many audiences over one pool of collected items

**Status:** Proposed

## Context

Today the pipeline narrows to a single digest: the aggregator judges the day's
items against one relevance profile, writes `digests/<day>.{md,json}`, and
every configured sink delivers that same file. That is correct for one reader
and wrong for the product ADR-0001 describes, whose reusability law says the
same three layers should be able to run a public newsletter alongside a
personal brief.

The requirement, from the maintainer: *"pull as many sources, and then have
sinks that selects a bunch of them to a different sink"*, and, decisively,
*"my boardgame newsletter can [be] a subset of board game items that I have in
my personal feed."* **So the outputs overlap. This is not a partition of the
day's items into disjoint buckets; the same item legitimately appears in a
public newsletter and in a private morning brief on the same day.**

That single sentence rules out the cheap design. Routing items to sinks — one
item, one destination — cannot express overlap, and a filter applied by each
sink to a shared digest cannot express voice: a newsletter written for
strangers and a brief written for one person differ in what they explain, not
only in which items survive. The narrowing that matters happens *before* the
digest is written, not after.

## Decision

**Introduce the EDITION: a named audience with its own source selection, its
own relevance profile, and therefore its own digest. Sinks deliver an edition
rather than "the digest".** Collection stays exactly as it is — one pool, one
copy of each item on disk — and editions are views over that pool.

```yaml
editions:
  personal:                      # the private brief
    profile: /etc/roozane/profile.md   # no `sources:` = the whole pool
  boardgames:                    # the public newsletter
    sources: [bgg-blog, bgg-hotness, dicebreaker]
    profile: /etc/roozane/boardgames.md

sinks:
  morning:
    edition: personal
    type: telegram
    params: {chat_id: "…"}
  archive:
    edition: personal            # same edition, second destination
    type: file
    params: {path: "/var/lib/roozane/out/{day}.md"}
  newsletter:
    edition: boardgames
    command: [/usr/local/lib/roozane-plugins/mailer]
```

1. **Selection is by SOURCE first, then by the edition's own profile.** The
   source list is deterministic, greppable and owned by the config; the profile
   does the fine-grained work the LLM is for.
   **The two layers are asymmetric, and saying they are simply "both needed"
   invites a reader to find the counterexample and doubt the section.** The
   source filter genuinely cannot substitute for the profile: `sources` cannot
   express "only the boardgame items inside a general feed". The profile *can*
   substitute for the source filter — prose saying "only items from the
   boardgame blogs" would mostly work — so **the source list earns its place on
   determinism, greppability and cost, not on expressiveness.** Its cost saving
   is likewise specific rather than general: it saves judging calls only on
   items an edition excludes wholesale, since an item two editions both select
   under different profiles is judged twice regardless. That is exactly the
   newsletter case, so the argument holds for a reason rather than in general.
   **Omitting `sources` selects the whole pool.** There is deliberately no
   `all` keyword: source ids are `[a-z0-9-]`, so `all` is a legal source id and
   a magic token would collide with a real source the day someone names one.
   Absence cannot collide.

2. **Overlap is free and expected.** An item selected by two editions is judged
   once per *distinct profile* and written into each edition's digest. Nothing
   deduplicates across editions, because a shared item is not an accident to be
   collapsed — it is the requirement.

   🚨 **This requires changing the judging cache, and it is a correctness
   requirement rather than an optimisation.** `state.json` currently keys a
   recorded judgement on the item filename alone (`state.Items[<filename>]`),
   so under editions the second profile would silently reuse the first
   profile's verdict — **a private profile's reasoning could be served verbatim
   into a public digest.** The key becomes (item filename, profile identity),
   where profile identity is a **hash of the profile's CONTENT**, so editing a
   profile invalidates its cached judgements instead of pinning stale ones.
   This is a `state.json` schema change: `stateSchema` bumps, and the existing
   schema-mismatch path already does the right thing by starting the day fresh,
   so the migration costs one re-judged day rather than a converter. It is
   listed in the migration notes below alongside the digest path.

3. **Editions each get a digest directory: `digests/<edition-id>/<day>.{md,json}`.**
   Edition ids take the same `[a-z0-9-]` constraint as source ids, for the same
   reason: they become path components.

4. **With no `editions:` block the engine behaves as one edition named
   `default`,** taking all sources and the top-level `relevance_profile`. This
   keeps the single-reader config — the one in ADR-0001's spirit — free of
   ceremony, and makes "one audience" the degenerate case of the general rule
   rather than a separate code path.

5. **A sink names exactly one edition** (`edition:`, defaulting to `default`).
   Many sinks may name the same one; that is how a digest reaches both a chat
   and a file. A sink naming an unknown edition fails at config load, unlike a
   sink `type`, because the edition list is closed and known locally.

6. **An edition that selects nothing, or whose items all fall below the bar,
   writes an empty digest** exactly as today (ADR-0002 §4), so a quiet edition
   still proves it ran.
   ⚠️ **Narrowing makes an empty digest weaker evidence than it was, and the
   ADR does not fix that.** A whole-pool digest went empty only if the whole
   day was quiet; an edition drawing on two sources goes empty the moment those
   two fail upstream, and the digest looks identical either way. ADR-0004's
   `ran/` markers still record which sources actually ran, so the information
   exists on disk — it is simply no longer visible in the digest a reader sees.
   **So the edition's digest records the collection outcome of the sources it
   selected** — which ran, which produced nothing, which failed. The
   information already exists at that point, both reviewers independently
   reached for it, and without it a narrow edition dilutes a total upstream
   failure into a digest that reads exactly like a quiet day.

## Consequences

- **This supersedes ADR-0002's closing answer to the same question.** That ADR
  said the public-newsletter case would be "a different data root with a
  different config… a second, fully independent pipeline". Editions replace
  that with one deployment over one pool, because two pipelines would collect
  every shared item twice and could never express an item belonging to both.
  A reader grepping for the newsletter case must not find the old answer
  unmarked.

- The reusability law becomes real: a public boardgame newsletter and a private
  brief run from one deployment, one collection pass, one set of files.
- **Cost scales with distinct PROFILES, not with editions or sinks.** Two
  editions sharing a profile share their per-item judgements; two editions with
  different profiles judge the overlapping items twice. This is the honest
  price of separate voices, and it is bounded by the number of audiences, which
  is small and human-chosen. Item judging is the expensive call, so an edition
  that merely re-selects an existing profile's sources is nearly free.
- **The digest path changes shape for everyone**, including the single-reader
  case: `digests/2026-09-04.md` becomes `digests/default/2026-09-04.md`. A
  one-time break, taken deliberately rather than special-casing the default
  edition into the old path, because a layout with one rule stays greppable and
  a layout with an exception does not. Files already written are left where
  they are.
- **Two pieces of retention and resume must be taught the new shape, and each
  fails SILENTLY until it is.** `pruneDigests` skips directory entries outright
  (`if e.IsDir() { continue }`), so the moment digests move into
  `digests/<edition>/` a configured `retention.digests` quietly becomes
  keep-forever — no error, just unbounded growth. And the `state.json` key
  change above must ship with the edition work, not after it, since the failure
  there is a wrong digest rather than a full disk. **Neither is a follow-up;
  both are part of the change that moves the paths.**
- **`config.example.yaml` has to move off `digests/`.** Its `local-file` sink
  writes `path: digests/{day}.md`, and the file sink resolves a path as given
  rather than against the data root, so run from the data root that example
  recreates a flat `digests/<day>.md` beside the new `digests/default/` —
  reintroducing by example the exact shape this ADR removes.
- Sinks get simpler to reason about, not harder: a sink is now "this edition,
  delivered there", and the question "which items does this sink send" is
  answered by the edition it names rather than by reading its params.
- **An edition is not a feedback surface.** Whether a reader can teach the
  engine by reacting to items stays open and unaffected; editions change who a
  digest is for, not how relevance is learned.
