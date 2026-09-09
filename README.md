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
collectors ─► days/<utc-day>/items/*.md ─► enrich ─► select ─► digests/<edition>/<day>.{md,json} ─► sinks
  (dumb)         raw text, provenance      (once)   (per       one digest per audience            (dumb)
                                                    edition)          │
                                                                      └─► reports/<day>.{md,json}
                                                                            why each item is or is not there
```

1. **Collectors** are deliberately dumb: a YAML config lists your sources, each with its own cadence
   (daily / weekly / monthly). Raw text is dumped as Markdown, unfiltered. Adding a source is a
   config change, never a code change. Newsletters arrive via a watched inbox folder that external
   tooling fills — the engine never touches a mailbox.
2. **The aggregator** is the only layer with a brain. It speaks the standard chat-completions API —
   strictly provider-agnostic, configured by endpoint and model names — and runs in two passes
   ([ADR-0005](docs/adr/0005-editions.md)). It reads each item **once, for nobody in particular**,
   recording a neutral summary, tags and a generic "is this substantive at all" score. Then it
   **selects per edition**: each audience narrows the shared pool by its own source list and its own
   relevance profile, and gets its own digest in its own voice. Suppression is the default. An
   edition may also cap how much of its digest any one subject takes and how many entries it carries
   ([ADR-0007](docs/adr/0007-subject-share.md), [ADR-0008](docs/adr/0008-fill-and-delivery.md)).
   Entries are filled one subject at a time rather than filtered in sequence, the digest only ever
   gets shorter, and every item left out is named in the report with which limit took it.
3. **Editions** are how one deployment serves several audiences from one pool of items — a public
   newsletter alongside a private brief. The same item may legitimately appear in both, and with no
   audience in the per-item record there is no private reasoning that could leak into a public
   digest. A config with no `editions:` block behaves as a single edition named `default`.
4. **The daily report** at `reports/<day>.{md,json}` is written after every edition, because it
   describes them. It says what each source yielded or how it failed, what the neutral pass made of
   each item, what every edition selected and why each remaining item was not, and what the run
   spent per pass and per model — in money too, if you tell it what your models cost. It is the
   first thing the engine writes for its owner rather than for a reader, and it is what makes the
   relevance profile tunable instead of guessed at.
5. **Sinks** deliver: a file, a chat message, later a podcast render. Each names either the edition
   it carries or the report. Dumb by design.

Collectors and sinks are **exec-pluggable** ([ADR-0003](docs/adr/0003-plugin-contract.md)): an
external program in any language can extend the edges over stdin/stdout, while the engine alone
owns the on-disk layout and its invariants ([ADR-0002](docs/adr/0002-on-disk-layout.md)).

### Writing a sink: render `markdown`, not `items[]`

A sink plugin receives a JSON envelope on stdin carrying two views of the same day, and **which one
it renders decides whether the reader gets a digest or a list**:

- **`markdown` is the digest the reader should see.** It is what the writing pass produced, and the
  only place where several reports of one event have been merged into a single entry.
- **`digest.items[]` is the structured record**: one entry per *article*, before that merge, with
  scores, tags and reasons attached. It is there for tooling that wants the data — filtering,
  archiving, counting.

🚨 **A sink that renders `items[]` will repeat itself.** Four articles about one occurrence are four
items and one entry, so a reader gets the same event four times and the digest's own merge never
reaches them. Nothing errors and nothing looks wrong from the engine's side, which is why it is
written here rather than left to be discovered: this exact mistake is what
[ADR-0008](docs/adr/0008-fill-and-delivery.md) exists to prevent.

The same holds for length. `items[]` carries every item the edition selected; `markdown` carries
what survived the digest's configured length and subject share. A sink that trims `items[]` itself
is imposing a second, different limit on top of the one the reader configured.

## Status

Early. The design ADRs are in [`docs/adr/`](docs/adr/); the layers are being built. See the issue
tracker for what exists yet.

## Configuration

See [`config.example.yaml`](config.example.yaml) — it is the product surface, and its comments are
the documentation.

### Upgrading from before editions

Digests used to be written flat, as `digests/<day>.md`. They now live under an
edition — `digests/<edition-id>/<day>.md`, and `digests/default/` for a config with no `editions:`
block.

**Files written by the older layout stay where they are, and the pruner will not touch them.** It
descends into edition directories and deliberately leaves anything sitting directly under `digests/`
alone, so a configured `retention.digests` window silently never applies to them — they accumulate
with no error. Move them once:

```
mkdir -p digests/default
find digests -maxdepth 1 -type f \( -name '*.md' -o -name '*.json' \) -exec mv {} digests/default/ \;
```

It moves only files sitting directly under `digests/`, so editions you already have are untouched,
and it stays quiet when there is nothing to move.

## File permissions: the engine refuses to start on a writable config

**Roozane requires that its config file, and every plugin executable named in it, is not writable by
group or other.** If either is, it refuses to start rather than warning
([ADR-0003 §8](docs/adr/0003-plugin-contract.md)).

This is not a hardening suggestion. **The config is the trust boundary**: whoever can edit it can
make the engine run any program as your user, and whoever can overwrite a configured plugin binary
gets the same thing without touching the config at all. A group-writable config file is therefore an
arbitrary-code-execution path for everyone in that group.

The check:

```
stat -c '%a %n' roozane.yaml
chmod go-w roozane.yaml
```

The refusal names the file, its mode and the fix, so a cron log is enough to act on:

```
config file (roozane.yaml) is writable by other users (mode 0664): whoever can write it can run
arbitrary code as this user, so the engine refuses to start (ADR-0003 §8) — fix with:
chmod go-w roozane.yaml
```

**The most common way to meet this is a first run, with no upgrade involved.** Git tracks only the
executable bit, so a fresh clone's files take their mode from your umask: `0644` under the usual
`022`, but `0664` under `002`. Copy `config.example.yaml` to `roozane.yaml` on a `umask 002` machine
and the very first run refuses.

The others:

- editing the config later on a machine with a different umask;
- restoring from a backup, or an `rsync` that dropped modes;
- mounting a config into the container from a host directory with looser modes;
- a plugin binary living in a group-writable directory — every source and sink `command[0]` is
  resolved through `PATH` and checked where it actually resolves to.

**Two limits, stated rather than implied away.** The check looks at the files, not at the
directories holding them — a world-writable directory still lets someone replace a `0644` file, and
directory hardening is deployment advice. And a command that cannot be found is not reported here: a
missing plugin fails at run time with a clearer message than this check could give, and refusing to
start over it would turn a broken sink into a dead engine.

## License

MIT.
