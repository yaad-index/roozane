# Contributing

## The gate before the code

Design decisions live in [`docs/adr/`](docs/adr/). If a change would contradict
an accepted ADR, the ADR is amended first — see ADR-0003, which opens by stating
which parts of ADR-0001 it reverses and why. An implementation that quietly
disagrees with a merged decision leaves the decision record lying.

## Running it

```sh
make build    # ./roozane
make check    # vet + test + lint — what CI runs
make test     # go test -race
make lint     # golangci-lint (pinned to the CI version in .github/workflows/ci.yml)
```

The three layers run independently, which is the point of the file-based handoff:

```sh
roozane collect                      # fetch what the config points at
roozane aggregate [-day YYYY-MM-DD]  # judge the day, write the digest
roozane deliver   [-day YYYY-MM-DD]  # send it to the configured sinks
```

## Pull requests

Merge commits are squashed, and the **PR title becomes the commit subject** —
so the conventional-commit prefix has to be in the title (`feat:`, `fix:`,
`docs:`, `chore:`). A PR titled without one contributes nothing to the next
release. See [RELEASING.md](RELEASING.md).

Tests use [testify](https://github.com/stretchr/testify) (`assert` / `require`).

## Testing: two rules that this codebase learned the hard way

Both of these came out of mutation runs on merged-looking code. They are written
down because the tests they replace all *looked* correct.

### 1. Assert what an observer sees, not what the mechanism leaves behind

When the property is about what someone else observes — atomicity, isolation,
ordering, visibility — test the observation. Side-effect assertions are proxies,
and a proxy the broken implementation also satisfies is not a test.

The engine writes every file through temp-then-rename so a reader never sees a
half-written document. The obvious test is "no leftover `.tmp` files". It is
worthless: a plain `os.WriteFile` leaves no temp file either, so the assertion
passes just as happily for the implementation it exists to rule out.

What works is racing a reader against the writer:

```go
// Write body A, then rewrite alternating A/B in a goroutine while the main
// loop reads. Every read must be one whole document.
require.True(t, string(raw) == bodyA || string(raw) == bodyB,
    "read a partial file: torn write (len %d)", len(raw))
```

Two details make it a real test rather than a hopeful one:

- **Bodies large enough that a non-atomic write cannot finish between the
  reader's open and its read** — half a megabyte is comfortable.
- **Assert the reader actually raced** (`assert.Greater(t, reads, 10, …)`).
  Without it, a loop that never overlapped the writer passes vacuously.

### 2. Every entry point needs its own test

A shared helper underneath is not where a regression lands. Roozane has three
places that write files atomically — `store.WriteItem`, `store.WriteAtomic`, and
the file sink — and **each one had this hole independently**:

| Writer | What the test asserted | What survived |
| --- | --- | --- |
| `store.WriteItem` | no leftover temp files | replacing the write with `os.WriteFile` |
| `store.WriteAtomic` | *(covered by the item test — it was not)* | the same replacement |
| file sink | no leftover temp files | the same replacement, on an operator-chosen path |

Three green suites, three real holes, one lesson learned three times. "The
underlying helper is tested" is not coverage of the caller; a change to the
caller is exactly what the mutation replaces.

### Checking a test is load-bearing

Break the code the test claims to protect and confirm the test fails. Copy the
file first — `git checkout -- file` discards every uncommitted change in the
working tree, and a clean `git status` afterwards makes that look like it worked.

```sh
cp internal/store/store.go /tmp/store.bak
# ...make the change the test should catch...
go test ./... ; cp /tmp/store.bak internal/store/store.go
```

A mutation that **survives** is the useful outcome: it means either the test is
asserting a proxy, or the code it targets does nothing. Both are worth knowing
before review, and both have been true here.
