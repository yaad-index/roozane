# ADR-0004: Empty collector runs leave a marker in the day folder

**Status:** Proposed

## Context

`due()` decides whether a source should be collected by asking the layout when
the source last wrote an item (ADR-0002: the layout is the only state layer 1
has). A fetch that legitimately returns zero items therefore records nothing,
reads as never-collected, and is re-fetched on every pass regardless of its
cadence — a weekly feed that is empty on its due day gets fetched on all seven
following days. Nothing breaks, but the cadence quietly stops meaning what the
config says, and the waste lands on someone else's server (#18).

Both reviewers of the collector core raised this and it was deliberately
deferred: recording "this source ran and found nothing" needs a home, and the
two candidates pull against different ADR-0002 commitments. Collector state in
a file would be direct, but ADR-0002 assigns `state.json` to the aggregator and
the layout's core property is that layer 1 is fully derivable from files a
human can `ls`. A marker in the day folder keeps that property but adds a file
kind ADR-0002 does not describe.

## Decision

**A successful fetch that yields zero items writes an empty marker file
`days/<day>/ran/<source-id>`, and `due()` reads the latest of (last item, last
marker) as the source's last run.** This extends ADR-0002's layout with one
sibling directory under the day folder:

```
days/
  2026-09-04/
    items/
      <source-id>--<key>.md
    ran/
      <source-id>            # empty file: source ran this day, found nothing
    state.json
```

1. **The marker is written only on a successful zero-item run.** A run that
   produced items needs no marker — the items themselves prove the run. A
   fetch that FAILED must not write one, so a failed source stays "not run"
   and is retried on the next pass: the marker means "ran fine, nothing
   there", and its absence stays ambiguous between "not due yet" and "failed",
   exactly as today. Silence and breakage remain distinguishable, which is the
   same principle the explicit empty-digest marker implements one layer up.

2. **`ran/` lives beside `items/`, not inside it.** The aggregator selects
   `items/*.md` and the inbox drain has its own ignore rules; a marker inside
   `items/` would be a non-item that every reader has to know to skip. A
   sibling directory means no existing reader changes at all — only `due()`
   learns to look in one more place.

3. **Markers are created atomically like every other write in this layout**
   (temporary name, then `rename(2)`), are empty (the name is the datum), and
   belong to their day folder, so the `retention.items` window covers them
   whenever day-folder pruning runs — they add no retention configuration.

4. **`state.json` stays the aggregator's file.** Layer 1 still owns no state
   file; its state remains the layout itself, now including the marker.

## Consequences

- Cadence means what the config says for empty sources: one fetch on the due
  day, not one per pass.
- Layer 1 stays fully derivable from the layout — `ls days/*/ran/` answers
  "which sources ran and found nothing" with no tooling, matching how every
  other question about layer 1 is answered.
- One new directory kind for external tools to know about; tools that only
  read `items/` or `digests/` are unaffected.
- A collector crash between fetch and marker write re-fetches once on the next
  pass — the failure mode is a duplicate fetch, never a skipped one.
- The cadence guarantee is conditional on retention: a `retention.items`
  window shorter than the longest configured cadence prunes the evidence
  (markers and items alike) before `due()` looks back one cadence period,
  returning such a source to per-pass fetching. This precondition predates
  this ADR — item-producing sources have the same dependency — and the default
  window (90) covers the largest built-in cadence (30). Rejecting such configs
  at validation is #25.
