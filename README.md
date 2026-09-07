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
