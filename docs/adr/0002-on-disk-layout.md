# ADR-0002: On-disk layout for the layer handoff

**Status:** Accepted

## Context

ADR-0001 fixes that the three layers communicate through files on disk and
defers the layout. The layout is a contract: collectors write it, the
aggregator reads it, sinks read the aggregator's output, and external tools
(the watched-folder ingest, future yaad-grove-style integrations) interact with
the engine only through these files. It has to be boring, predictable, and
greppable by a human with no tooling.

## Decision

1. **One configurable data root; everything lives under it. All day keys are
   UTC.** The day-folder name is derived from the item's `fetched_at` in UTC —
   the same clock the front-matter uses — so collection, aggregation and
   retention share one boundary and a midnight is unambiguous. (A digest MAY
   present times in the reader's timezone; the disk speaks UTC only.)

   ```
   <data>/
     inbox/                       # watched folder: external tools drop files here
     days/
       2026-09-04/                # UTC day
         items/
           <source-id>--<key>.md  # one file per collected item
         state.json               # aggregator bookkeeping for the day
     digests/
       2026-09-04.md              # the aggregator's human-readable output
       2026-09-04.json            # the same digest, structured, for sinks
   ```

2. **One Markdown file per collected item**, named `<source-id>--<key>.md`.
   `source-id` is the source's config key, **constrained to `[a-z0-9-]`**
   since it becomes a path component. `<key>` is a **stable digest (first 12
   hex chars of SHA-256) of the item's identity** — its URL when it has one,
   otherwise its content — so re-running a collector for the same (source,
   day) rewrites the same filenames instead of appending duplicates: the
   collector is idempotent by construction, not by convention. **Item writes
   are atomic like every other write in this layout: create under a temporary
   name, `rename(2)` into `items/`** — a re-run must never expose a torn file
   to a concurrently reading aggregator. The file has
   YAML front-matter followed by the raw extracted text:

   ```markdown
   ---
   source: hn-frontpage          # config key
   url: https://…                # where it came from, when applicable
   title: …                      # the source's own title, when available
   fetched_at: 2026-09-04T06:30:00Z
   collector: feed               # which collector type produced it
   ---
   (raw extracted text, unmodified)
   ```

   Front-matter carries provenance only. Nothing in layer 1 summarizes,
   scores, or trims.

3. **The inbox is drained into the day folder by the collector run.** Files
   dropped into `inbox/` (newsletters forwarded by an external tool, arbitrary
   hand-offs) are normalized on the next run into `items/` files with
   `collector: inbox` and the original filename recorded in front-matter, and
   **the original is deleted after the normalized item is atomically in
   place** — the inbox is a queue, not an archive. **Producers must write
   atomically: create under a temporary name (dotfile or `*.tmp`) and
   `rename(2)` into `inbox/`.** The drain ignores dotfiles and `*.tmp`, which
   is what makes a half-written drop unreadable by contract rather than by
   luck. The engine never reaches outside its data root to find input.

4. **The digest is written twice, deliberately, into its own sibling tree
   (`digests/`).** `<day>.md` is the human-readable artifact;
   `<day>.json` is the sink input (structured items with source references,
   so a sink can link back or select subsets). Both are written atomically at
   the end of an aggregator run; a day with nothing above the relevance bar
   produces both files with an explicit empty marker rather than no file, so
   "quiet day" and "aggregator never ran" stay distinguishable. Living outside
   `days/` is what lets digests outlive the raw items they came from.

5. **`state.json` records what the aggregator has processed** (per-item status,
   model used, token counts), so a re-run resumes instead of re-paying for the
   whole day, and failures are inspectable per item.

6. **Retention is two configured windows, both counted in UTC days.**
   `retention.items` (default 90) prunes `days/` folders; `retention.digests`
   (default 0 = keep forever) prunes the `digests/` tree separately. The
   digests are small and are the record worth keeping, and they no longer live
   inside the folder the item-prune deletes.

## Consequences

- Filenames derived from a stable identity key make the collector idempotent
  per (source, day) and safe to re-run; the layout stays immutable across
  days.
- The explicit empty-digest marker implements the "silence must be
  distinguishable from breakage" principle at the file level.
- `digests/<day>.json` is a second schema to keep stable; sinks depend on it. Its
  exact schema is fixed when layer 3's first sink is built (issue #8), and it
  versions with a top-level `schema` field from day one.
- The per-day folder is the natural unit for the future public-newsletter
  reuse: a different data root with a different config is a second, fully
  independent pipeline.
