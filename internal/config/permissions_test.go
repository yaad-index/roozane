package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// execConfig is the baseline with one exec source pointing at path.
func execConfig(path string) string {
	return `
data_root: /srv/roozane
relevance_profile: /srv/roozane/profile.md
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
  plugin-source:
    collector: exec
    cadence: daily
    command: ["` + path + `"]
`
}

// writeMode writes a file at an exact mode, defeating the umask, so a
// permission test asserts the mode it names rather than the one the running
// user's umask happens to allow.
func writeMode(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, body, mode))
	require.NoError(t, os.Chmod(path, mode))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode, info.Mode().Perm(), "vacuity guard: the fixture must really be at the mode under test")
}

func TestLoadRefusesAWorldWritableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roozane.yaml")
	writeMode(t, path, []byte(validConfig), 0o666)

	_, err := Load(path)
	require.Error(t, err, "a config anyone can edit is arbitrary code execution as this user")

	msg := err.Error()
	assert.Contains(t, msg, path, "the message must name the offending path")
	assert.Contains(t, msg, "0666", "and the mode it found")
	assert.Contains(t, msg, "chmod go-w "+path, "and the fix, since this is first read in a cron log")
}

func TestLoadRefusesAGroupWritableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roozane.yaml")
	writeMode(t, path, []byte(validConfig), 0o664)

	_, err := Load(path)
	require.Error(t, err, "group-writable is other-writable too: a shared group is still not just this user")
}

func TestLoadAcceptsAnOwnerOnlyWritableConfig(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644, 0o640, 0o444} {
		path := filepath.Join(t.TempDir(), "roozane.yaml")
		writeMode(t, path, []byte(validConfig), mode)

		_, err := Load(path)
		assert.NoError(t, err, "mode %#o is not writable by anyone else", mode)
	}
}

func TestLoadRefusesAWorldWritablePluginExecutable(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "collector-plugin")
	writeMode(t, binary, []byte("#!/bin/sh\nexit 0\n"), 0o777)

	path := filepath.Join(dir, "roozane.yaml")
	writeMode(t, path, []byte(execConfig(binary)), 0o600)

	_, err := Load(path)
	require.Error(t, err, "whoever can overwrite the binary gets what whoever can edit the config gets")

	msg := err.Error()
	assert.Contains(t, msg, binary)
	assert.Contains(t, msg, `source "plugin-source"`, "the message must name which entry is at fault")
	assert.Contains(t, msg, "chmod go-w "+binary)
}

func TestLoadAcceptsAnOwnerOnlyPluginExecutable(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "collector-plugin")
	writeMode(t, binary, []byte("#!/bin/sh\nexit 0\n"), 0o755)

	path := filepath.Join(dir, "roozane.yaml")
	writeMode(t, path, []byte(execConfig(binary)), 0o600)

	_, err := Load(path)
	assert.NoError(t, err, "0755 is executable by all but writable only by its owner")
}

// TestCheckPermissionsCoversSinksToo — ADR-0003 §8 is engine-wide: a writable
// sink plugin is the same hole as a writable collector plugin.
func TestCheckPermissionsCoversSinksToo(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "sink-plugin")
	writeMode(t, binary, []byte("#!/bin/sh\nexit 0\n"), 0o775)

	body := validConfig + `
sinks:
  plugin-sink:
    command: ["` + binary + `"]
`
	path := filepath.Join(dir, "roozane.yaml")
	writeMode(t, path, []byte(body), 0o600)

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `sink "plugin-sink"`)
	assert.Contains(t, err.Error(), binary)
}

// TestCheckPermissionsIgnoresAMissingExecutable — a missing plugin is a runtime
// failure with a clearer message than this check could give, and refusing to
// start over it would turn a broken sink into a dead engine.
func TestCheckPermissionsIgnoresAMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roozane.yaml")
	writeMode(t, path, []byte(execConfig("/nonexistent/collector-plugin")), 0o600)

	_, err := Load(path)
	assert.NoError(t, err, "a path that does not exist is not a permission finding")
}

// TestCheckPermissionsReportsEveryOffender — one pass should let an operator fix
// everything at once rather than rerunning to find the next one.
func TestCheckPermissionsReportsEveryOffender(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "plugin-a")
	second := filepath.Join(dir, "plugin-b")
	writeMode(t, first, []byte("#!/bin/sh\nexit 0\n"), 0o777)
	writeMode(t, second, []byte("#!/bin/sh\nexit 0\n"), 0o777)

	body := validConfig + `
sinks:
  a-sink:
    command: ["` + first + `"]
  b-sink:
    command: ["` + second + `"]
`
	path := filepath.Join(dir, "roozane.yaml")
	writeMode(t, path, []byte(body), 0o666)

	_, err := Load(path)
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, path, "the config itself")
	assert.Contains(t, msg, first)
	assert.Contains(t, msg, second)
}
