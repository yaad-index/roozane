# Multi-stage build: a static, CGO-free binary in the builder, copied into a
# minimal non-root runtime that carries the binary and nothing else.
#
# Roozane is run-and-exit by design — each subcommand performs one pass and
# terminates — so this image deliberately contains NO loop and NO cron. The
# entrypoint is the binary itself and the caller supplies the subcommand, which
# keeps a single pass invocable on its own and lets the scheduler see each run's
# exit code instead of watching a process that never returns.

# --- builder ---
# Base images are pinned by digest (reproducible, tamper-evident); the readable
# tag sits on the comment line above each digest. Both digests are multi-arch
# manifest-list digests, so an arm64 + amd64 publish build resolves each platform.
# golang:1.26.2
FROM golang@sha256:b54cbf583d390341599d7bcbc062425c081105cc5ef6d170ced98ef9d047c716 AS build
WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION stamps the version at link time. The publish workflow passes it as a
# build-arg on a tagged release (a pure semver) and a non-empty value is used
# verbatim. Left EMPTY for a source build, where it is derived below from the
# .git copied into THIS builder stage — the runtime stage copies only the binary,
# so .git never reaches the published image.
ARG VERSION=
# git describe reads the .git in the build context; mark it safe so git does not
# refuse it as dubious-ownership under the build user.
RUN git config --global --add safe.directory /src
# CGO off → a fully static binary, which is what lets the distroless static base
# work. -trimpath keeps build paths out of the binary; -s -w strip debug info.
#
# On a source build the nearest release tag is derived so the image reports a
# legible version — 0.2.0 at a tag, 0.2.0-3-gabc1234 ahead of one, -dirty when
# the tree was modified — rather than a bare "dev". --match keeps it to release
# tags and the leading v is stripped, so a source build and a release build
# report the same shape. --always still yields a bare commit when no tag is
# reachable (a shallow clone has none), which reads as distinct-from-a-version
# rather than as a wrong one.
RUN VERSION="${VERSION:-$(git describe --tags --always --dirty --match 'v*' 2>/dev/null)}"; \
    VERSION="${VERSION#v}"; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/roozane ./cmd/roozane

# --- runtime ---
# The static base carries CA certificates, which the collectors and the
# aggregator need for outbound HTTPS. It carries no shell and no package
# manager, so there is nothing in the image to run but the binary.
#
# No tzdata is needed: the layout and every day key are UTC throughout.
# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/roozane /usr/local/bin/roozane
USER nonroot:nonroot

# No CMD: run with no subcommand and the binary prints its usage, which is the
# most useful thing an image with no default action can do. The caller names the
# pass it wants — `collect`, `aggregate` or `deliver` — per run.
#
# The config file and the data root come from outside the image. Give both as
# absolute paths: the working directory is / and the runtime user cannot write
# there, so a relative data_root would fail at the first write.
#
#   docker run --rm \
#     -v /srv/roozane/roozane.yaml:/etc/roozane/roozane.yaml:ro \
#     -v /srv/roozane/data:/var/lib/roozane \
#     ghcr.io/yaad-index/roozane:latest collect -config /etc/roozane/roozane.yaml
ENTRYPOINT ["/usr/local/bin/roozane"]
