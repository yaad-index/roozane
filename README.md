# Roozane (روزنه)

*The aperture the day's light comes through.*

Roozane is a self-hosted personal news engine. **You** choose the sources — feeds, news sites, web
searches, newsletters — and it distills them into a short digest of only what you care about,
delivered however you like: a file, a chat message, a podcast.

It exists because platform-curated digests fail in a predictable way: the platform picks the topics
and the output has a fixed length, so you spend ten minutes listening for one minute of relevance.
Roozane inverts both:

- **You own the relevance profile.** A prose file you edit; the engine never infers it.
- **Length follows signal, and zero is valid.** On a quiet day the correct digest is empty.

## How it works

Three layers, one binary, files in between ([ADR-0001](docs/adr/0001-foundations.md)):

```
collectors  ──►  days/<utc-day>/items/*.md  ──►  aggregator  ──►  digests/<day>.{md,json}  ──►  sinks
 (dumb)              raw text, provenance        (the brain)          the day's digest         (dumb)
```

1. **Collectors** are deliberately dumb: a YAML config lists your sources, each with its own cadence
   (daily / weekly / monthly). Raw text is dumped as Markdown, unfiltered. Adding a source is a
   config change, never a code change. Newsletters arrive via a watched inbox folder that external
   tooling fills — the engine never touches a mailbox.
2. **The aggregator** is the only layer with a brain. It speaks the standard chat-completions API —
   strictly provider-agnostic, configured by endpoint and model names — reads the day's items one at
   a time against your relevance profile, and writes the digest. Suppression is the default.
3. **Sinks** deliver: a file, a chat message, later a podcast render. Dumb by design.

Collectors and sinks are **exec-pluggable** ([ADR-0003](docs/adr/0003-plugin-contract.md)): an
external program in any language can extend the edges over stdin/stdout, while the engine alone
owns the on-disk layout and its invariants ([ADR-0002](docs/adr/0002-on-disk-layout.md)).

## Status

Early. The design ADRs are in [`docs/adr/`](docs/adr/); the layers are being built. See the issue
tracker for what exists yet.

## Configuration

See [`config.example.yaml`](config.example.yaml) — it is the product surface, and its comments are
the documentation.

## License

MIT.
