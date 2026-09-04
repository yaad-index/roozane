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

1. **Plugins never touch the data root. The engine owns the disk.** A collector
   plugin produces items on **stdout**; the engine parses them and performs the
   ADR-0002 writes itself (day key, identity digest, temp-then-rename, charset).
   A sink plugin receives the digest on **stdin** and performs delivery. This
   keeps every filesystem invariant in exactly one implementation and makes a
   plugin a pure transformer: bytes in, bytes out, exit code.

2. **Collector plugins: NDJSON out.** The engine invokes the configured
   command with the source's config entry as JSON on stdin, plus a contract
   header (`{"contract": 1, "source": "<source-id>"}` merged into it). The
   plugin writes one JSON object per line to stdout:

   ```json
   {"url": "https://…", "title": "…", "fetched_at": "2026-09-04T06:30:00Z", "content": "raw text"}
   ```

   `content` is required; the rest is provenance and optional. `fetched_at`
   defaults to the engine's clock when omitted. Malformed lines are logged and
   skipped, not fatal — one bad item must not cost the batch.

3. **Sink plugins: digest JSON in.** The engine invokes the configured command
   with the day's `digests/<day>.json` on stdin (the versioned schema from
   ADR-0002). Anything the sink writes to stdout is logged verbatim; delivery
   success is the exit code.

4. **Exit codes and stderr are the whole error channel.** `0` is success;
   anything else marks the source/sink run failed in `state.json` (collectors)
   or the run log (sinks), with stderr captured. There is no partial-success
   protocol in contract 1; a collector that emitted items before failing still
   has those items kept (they were valid when parsed).

5. **Timeouts are per source/sink, configured, with a sane default.** On
   expiry the engine kills the process group and records a timeout failure.
   A hung plugin must never hold the day's run hostage.

6. **Environment is explicit.** Plugins receive a minimal environment: `PATH`,
   locale, and only the variables the config entry names (for credentials, by
   reference — the config carries variable *names*, never secret values).
   Nothing else leaks through.

7. **The contract is versioned from day one.** `contract: 1` in the stdin
   header; a plugin that needs a newer contract fails loudly rather than
   misparsing. Built-in collectors (feed, http, inbox) use the same internal
   interface so the contract stays honest — the engine is its own first
   plugin consumer.

8. **Security stance, stated plainly: a plugin runs with the engine's
   privileges, and the config is the trust boundary.** Whoever can edit the
   config can run arbitrary code as the engine's user — that is inherent to
   exec-based extension and is documented rather than hidden. Contract 1 does
   no sandboxing; process-level isolation (separate user, cgroups, containers)
   is deployment advice, and any in-engine sandboxing would be a later ADR.
   The engine refuses to run plugins when the config file is writable by
   other users (mode check at startup), which catches the cheapest form of
   privilege drift.

## Consequences

- Keeping the disk engine-owned means plugin authors cannot corrupt layout
  invariants, at the cost that plugins cannot stream arbitrarily huge items
  (stdout passes through the engine's parser; a size cap per item is a config
  knob with a generous default).
- NDJSON-over-stdio is implementable in any language with no library, which is
  the point of exec-based edges.
- The no-partial-success rule keeps contract 1 simple; if a source needs
  resumable partial fetches, that pressure lands on a contract 2, not on
  ad-hoc flags.
- The config-is-the-trust-boundary stance makes config file permissions part
  of the security model; the startup mode check enforces the cheap half and
  documentation carries the rest.
