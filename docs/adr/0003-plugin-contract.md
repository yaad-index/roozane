# ADR-0003: Exec-based plugin contract for collectors and sinks

**Status:** Proposed

## Context

ADR-0001 fixes that the pipeline's edges are open — collectors and sinks may be
external programs in any language — and that the middle is not. ADR-0002 fixes
the on-disk layout and its invariants: UTC day keys, identity-keyed filenames,
atomic writes, the constrained `source-id` charset. The plugin contract has to
extend the edges without letting a plugin break those invariants, and it has to
be honest about the security consequences of running configured executables.

## Decision

> **This ADR amends ADR-0001 §2/§5.** ADR-0001 described collectors as dumping
> Markdown files themselves ("config in, files out"). Decision 1 below reverses
> that deliberately: collector plugins produce items on stdout and the ENGINE
> performs the writes ADR-0001 §2 described, so every ADR-0002 filesystem
> invariant has exactly one implementation. ADR-0001 remains accurate as the
> historical record; this is the operative contract.

1. **Plugins never touch the data root. The engine owns the disk.** A collector
   plugin produces items on **stdout**; the engine parses them and performs the
   ADR-0002 writes itself (day key, identity digest, temp-then-rename, charset).
   A sink plugin receives the digest on **stdin** and performs delivery. This
   keeps every filesystem invariant in exactly one implementation and makes a
   plugin a pure transformer: bytes in, bytes out, exit code.

2. **Collector plugins: NDJSON out.** The engine invokes the configured
   command with a nested envelope on stdin — the header never collides with
   plugin parameters:

   ```json
   {"contract": 1, "source": "<source-id>", "params": { …the source's params… }}
   ```

   The plugin writes one JSON object per line to stdout:

   ```json
   {"url": "https://…", "title": "…", "source_time": "2026-09-04T06:30:00Z", "content": "raw text"}
   ```

   `content` is required; the rest is provenance and optional. **The engine
   stamps `fetched_at` with its own clock at ingestion — always.** A
   plugin-supplied timestamp is preserved as `source_time` provenance and
   never chooses the day key, so a plugin cannot write into an immutable past
   day (the ADR-0002 day key derives from the engine-stamped `fetched_at`).
   Malformed lines are logged and skipped, not fatal — one bad item must not
   cost the batch. **Items are size-capped: `max_item_bytes`, a config knob
   with a 1 MiB default; an oversize item is truncated with a marker and
   logged**, since the whole stream passes through the engine's parser.

3. **Sink plugins: digest JSON in, with the same envelope.** Sinks are
   configured in a `sinks:` section parallel to `sources:` (id, command/type,
   `params`, `env`) — **a config-schema addition this ADR introduces**, so a
   chat sink's destination id has a place to live. The engine invokes the
   configured command with:

   ```json
   {"contract": 1, "sink": "<sink-id>", "params": { … }, "digest": { …the ADR-0002 digest JSON… }}
   ```

   Anything the sink writes to stdout is logged verbatim; delivery success is
   the exit code.

4. **Exit codes and stderr are the whole error channel.** `0` is success;
   anything else marks the source/sink run failed in `state.json` (collectors)
   or the run log (sinks), with stderr captured. There is no partial-success
   protocol in contract 1; a collector that emitted items before failing still
   has those items kept (they were valid when parsed).

5. **Timeouts are per source/sink, configured, with a sane default.** The
   engine **starts each plugin in its own process group (setpgid)** and on
   expiry kills that group — created by the engine precisely so the kill can
   never reach the engine itself — and records a timeout failure. A hung
   plugin must never hold the day's run hostage.

6. **Environment is explicit.** Plugins receive a minimal environment: `PATH`,
   locale, and only the variables named in the entry's **`env:` allow-list —
   an optional per-source/per-sink config field this ADR adds to the schema**
   (credentials by reference: the config carries variable *names*, never
   secret values). Nothing else leaks through.

7. **The contract is versioned from day one.** `contract: 1` in the stdin
   header; a plugin that needs a newer contract fails loudly rather than
   misparsing. Built-in collectors (feed, http) use the same internal
   interface so the contract stays honest — the engine is its own first
   plugin consumer. (The inbox is not a collector: it is the ADR-0002 drain.)

8. **Security stance, stated plainly: a plugin runs with the engine's
   privileges, and the config is the trust boundary.** Whoever can edit the
   config can run arbitrary code as the engine's user — that is inherent to
   exec-based extension and is documented rather than hidden. Contract 1 does
   no sandboxing; process-level isolation (separate user, cgroups, containers)
   is deployment advice, and any in-engine sandboxing would be a later ADR.
   At startup the engine refuses to run when the config file — or any
   configured plugin executable — is writable by other users, which catches
   the cheapest form of privilege drift on both the instructions and the code
   they point at.

## Consequences

- Keeping the disk engine-owned means plugin authors cannot corrupt layout
  invariants, at the cost that plugins cannot stream arbitrarily huge items
  (the `max_item_bytes` cap in decision 2 is the explicit price).
- NDJSON-over-stdio is implementable in any language with no library, which is
  the point of exec-based edges.
- The no-partial-success rule keeps contract 1 simple; if a source needs
  resumable partial fetches, that pressure lands on a contract 2, not on
  ad-hoc flags.
- The config-is-the-trust-boundary stance makes config file permissions part
  of the security model; the startup mode check enforces the cheap half and
  documentation carries the rest.
