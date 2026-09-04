# ADR-0002: On-disk layout for the layer handoff

**Status:** Proposed

## Context

ADR-0001 fixes that the three layers communicate through files on disk and
defers the layout. The layout is a contract: collectors write it, the
aggregator reads it, sinks read the aggregator's output, and external tools
(the watched-folder ingest, future yaad-grove-style integrations) interact with
the engine only through these files. It has to be boring, predictable, and
greppable by a human with no tooling.

## Decision

1. **One configurable data root; everything lives under it.**

   ```
   <data>/
     inbox/                      # watched folder: external tools drop files here
     days/
       2026-09-04/
         items/
           <source-id>--<seq>.md # one file per collected item
         digest.md               # the aggregator's human-readable output
         digest.json             # the same digest, structured, for sinks
         state.json              # aggregator bookkeeping for the day
   ```

2. **One Markdown file per collected item**, named `<source-id>--<seq>.md`,
   where `source-id` is the config key of the source and `seq` is a
   monotonically increasing number within that source and day. The file has
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
   `collector: inbox` and the original filename recorded in front-matter. The
   engine never reaches outside its data root to find input.

4. **The digest is written twice, deliberately.** `digest.md` is the
   human-readable artifact (what a reader or a piping tool sees);
   `digest.json` is the sink input (structured items with source references,
   so a sink can link back or select subsets). Both are written atomically at
   the end of an aggregator run; a day with nothing above the relevance bar
   produces both files with an explicit empty marker rather than no file, so
   "quiet day" and "aggregator never ran" stay distinguishable.

5. **`state.json` records what the aggregator has processed** (per-item status,
   model used, token counts), so a re-run resumes instead of re-paying for the
   whole day, and failures are inspectable per item.

6. **Retention is a configured number of days.** A cleanup pass deletes day
   folders older than the retention window. Default generous (90 days); the
   digest files may optionally be retained longer than `items/`, since they are
   small and are the record worth keeping.

## Consequences

- The layout is append-only within a day and immutable across days, which
  makes the collector idempotent per (source, day) and safe to re-run.
- The explicit empty-digest marker implements the "silence must be
  distinguishable from breakage" principle at the file level.
- `digest.json` is a second schema to keep stable; sinks depend on it. Its
  exact schema is fixed when layer 3's first sink is built (issue #8), and it
  versions with a top-level `schema` field from day one.
- The per-day folder is the natural unit for the future public-newsletter
  reuse: a different data root with a different config is a second, fully
  independent pipeline.
