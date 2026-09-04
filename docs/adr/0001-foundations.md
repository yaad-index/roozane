# ADR-0001: Foundations

**Status:** Proposed

## Context

Roozane (روزنه, "aperture") is a self-hosted, open-source personal news engine.
The user chooses their own sources — feeds, news sites, web searches, newsletters —
and the engine distills them into a short digest of only what that user cares
about, delivered however they like. It exists because platform-curated digests
fail in a specific way: the platform picks the topics and the output has a fixed
length, so the reader spends ten minutes on one minute of relevance.

Two product laws follow from that failure and shape everything below:

- **The user owns the relevance profile.** Source selection and interest
  weighting are editable configuration, never an opaque inference.
- **Length follows signal, and zero is a valid length.** The digest contains
  only the main data points; on a quiet day the correct output is nothing.

The engine must also run unattended on the owner's own infrastructure,
independent of any interactive agent or session, and be reusable: the same
pipeline that serves one person's news should be able to run a topical public
newsletter later, purely by swapping configuration.

## Decision

1. **Three-layer pipeline, one project: collect → aggregate → deliver.**
   The layers communicate through files on disk, not through in-process calls,
   so each layer can be run, tested, and replaced independently.

2. **Layer 1 — collectors are deliberately dumb. No AI, no ranking, no
   filtering.** A YAML configuration lists sources; each entry carries its own
   fetch cadence (daily / weekly / monthly). A collector fetches however the
   source requires (HTTP fetch, feed parse, headless browser, an external
   command) and dumps raw text as **Markdown files into a per-day folder**.
   Adding a source is a config change, never a code change. Newsletters reach
   the engine as files pushed into a watched folder by an external tool; the
   engine never touches a mailbox.

3. **Layer 2 — the aggregator is the only layer with a brain, and it is
   deliberately stiff for now.** It reads the day's Markdown files one at a
   time and makes **one LLM API call per unit of work** (read, extract the few
   load-bearing data points, judge relevance against the profile). The LLM is
   reached through the **standard chat-completions API shape, strictly
   provider-agnostic**: the aggregator carries its own configuration (base URL,
   model, credentials), its own prompts, and its own context handling, and the
   engine neither endorses nor depends on any particular provider. Cost scales
   with the task by configuring a small model per item and a larger one for the
   final digest. No plugin system in this layer. The profile and the reader's
   ongoing feedback are inputs to the prompt, and the suppression default is
   built in: an item that does not clear the relevance bar produces nothing.

4. **Layer 3 — sinks are dumb and pluggable.** The digest goes out as a podcast
   render, a chat message (Telegram or similar), an e-mail/newsletter, or a
   file. Digest in, delivery out, no intelligence.

5. **Pluggability at the edges is exec-based.** Collectors and sinks may be
   external programs in any language, invoked with a defined contract (config
   in, files out / digest in, delivery out), so foreign code extends the engine
   without modifying it. The exact plugin contract is a later ADR; this ADR
   fixes only that the edges are open and the middle is not.

6. **Backend in Go, shipped as a single binary**, consistent with the other
   projects in this organization. Small operational surface, easy to self-host,
   cron-friendly.

7. **License: MIT. Public repository.** Open source from day one, matching the
   organization's conventions, with release automation (release-please) and CI
   ported from the existing projects rather than reinvented.

## Consequences

- File-based handoff makes each layer independently testable and lets a failed
  layer be re-run without re-fetching the world, at the cost of defining the
  on-disk layout carefully (a later ADR).
- The one-call-per-item aggregator is simple and debuggable, but serial; if
  daily volume grows, batching or concurrency becomes a measured optimization,
  not a redesign.
- Exec-based edge plugins mean the engine's security boundary includes whatever
  the owner configures it to run; the plugin contract ADR must treat that
  explicitly.
- Keeping preferences in configuration is what makes the future public-newsletter
  use a configuration exercise instead of a fork.
- The relevance profile's quality decides the product's value; seeding and
  updating it (including how reader feedback flows back in) needs its own
  design pass.
