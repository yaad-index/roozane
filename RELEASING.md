# Releasing

Roozane uses [release-please](https://github.com/googleapis/release-please) to
automate versioning and releases from [Conventional
Commits](https://www.conventionalcommits.org/). Versions are git tags
(`vMAJOR.MINOR.PATCH`).

## How it works

1. Merge PRs to `main` using conventional-commit titles (`feat:`, `fix:`,
   `docs:`, `chore:`, …). Pre-1.0, `feat:` bumps the minor version, `fix:` the
   patch, and a breaking change bumps the minor rather than the major
   (`bump-minor-pre-major`).
2. The `release-please` workflow keeps an open **release PR** that accumulates
   the changelog and the next version. It updates itself as more commits land.
3. When you want to cut the release, get the release PR reviewed and merged
   (same gate as any PR: CI green + approvals). Merging it creates the tag, the
   GitHub Release, and updates `CHANGELOG.md`.

The conventional prefix has to be in the **PR title**, because merges are
squashed and the PR title becomes the commit subject. A PR titled without a
prefix contributes nothing to the next version.

## The first release is pinned to 0.1.0

`initial-version: "0.1.0"` in `release-please-config.json` sets the bootstrap
version. Without it, release-please cuts the very first feature-bearing release
as `1.0.0` regardless of commit types — `bump-minor-pre-major` does not change
that, since it governs increments once already below 1.0.0, not the bootstrap.

Verify this on the live release PR rather than trusting the config: once the
first `feat:` lands on `main`, check that the release PR's title proposes
`0.1.0`. If it proposes `1.0.0`, the fallback is a one-time `"release-as":
"0.1.0"` in the same config block — which must be **removed immediately after
the first release is cut**, because it pins every later release computation to
that same version.

## First release: the one-time manual step

The release PR is opened by the built-in `GITHUB_TOKEN`, and GitHub
deliberately does **not** run workflows for events triggered by that token. So
the required `check` status does not run on the release PR on its own, and
branch protection won't let it merge until it does.

Until the optional token below is configured, do this once per release PR:

- **Close the release PR, then reopen it.** Reopening it as a real user fires
  the `pull_request` event, so `check` runs. Then approve and merge as usual.

## Optional: remove the manual step

Add a repository secret named `RELEASE_PLEASE_TOKEN` — a fine-grained personal
access token or a GitHub App token with `contents: write` and
`pull_requests: write`. The workflow picks it up automatically (no workflow
edit): the release PR is then authored by that identity, `check` runs normally,
and cutting a release becomes a plain CI-green + approvals merge with no
close/reopen.
