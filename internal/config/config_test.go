package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig is the baseline every test starts from: the smallest config that
// passes, so a test that expects a failure is provably failing on the one thing
// it changed.
const validConfig = `
data_root: /srv/roozane
relevance_profile: /srv/roozane/profile.md
retention:
  items: 30
  digests: 365
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models:
    item: small
    digest: large
  timeout: 45s
sources:
  example-feed:
    collector: feed
    cadence: daily
    params:
      url: https://example.com/feed.xml
  example-site:
    collector: http
    cadence: weekly
`

// write drops body into a temp dir and returns the config path, so each test
// loads through the real Load path rather than a parse-only shortcut.
func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "roozane.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, validConfig))
	require.NoError(t, err)

	assert.Equal(t, "/srv/roozane", cfg.DataRoot)
	assert.Equal(t, "/srv/roozane/profile.md", cfg.RelevanceProfile)
	assert.Equal(t, 30, cfg.Retention.ItemDays())
	assert.Equal(t, 365, cfg.Retention.Digests)
	assert.Equal(t, "https://api.example.com/v1", cfg.Aggregator.BaseURL)
	assert.Equal(t, "ROOZANE_API_KEY", cfg.Aggregator.APIKeyEnv)
	assert.Equal(t, "small", cfg.Aggregator.Models.Item)
	assert.Equal(t, "large", cfg.Aggregator.Models.Digest)
	assert.Equal(t, 45*time.Second, cfg.Aggregator.RequestTimeout())

	require.Len(t, cfg.Sources, 2)
	assert.Equal(t, "feed", cfg.Sources["example-feed"].Collector)
	assert.Equal(t, CadenceDaily, cfg.Sources["example-feed"].Cadence)
	assert.Equal(t, CadenceWeekly, cfg.Sources["example-site"].Cadence)
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, `
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models:
    item: small
    digest: large
sources:
  example-feed:
    collector: feed
    cadence: daily
`))
	require.NoError(t, err)

	assert.Equal(t, defaultDataRoot, cfg.DataRoot)
	assert.Equal(t, defaultRelevanceProfile, cfg.RelevanceProfile)
	assert.Equal(t, defaultRetentionItems, cfg.Retention.ItemDays())
	assert.Equal(t, defaultTimeout, cfg.Aggregator.RequestTimeout())
	// Zero digest retention means keep forever, so it must stay zero rather
	// than being "defaulted" into a pruning window nobody asked for.
	assert.Zero(t, cfg.Retention.Digests)
}

func TestLoadResolvesRelativePaths(t *testing.T) {
	path := write(t, `
data_root: ./var
relevance_profile: interests.md
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
  example-feed: {collector: feed, cadence: daily}
`)
	dir := filepath.Dir(path)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(dir, "var"), cfg.DataRootPath())
	assert.Equal(t, filepath.Join(dir, "interests.md"), cfg.RelevanceProfilePath())
	// An absolute path is taken as given.
	cfg.DataRoot = "/elsewhere"
	assert.Equal(t, "/elsewhere", cfg.DataRootPath())
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load(write(t, validConfig+"\nunexpected_key: 1\n"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected_key",
		"a misspelled key must fail loudly; silently ignoring it is how an edit does nothing for no visible reason")
}

func TestLoadRejectsUnknownSourceField(t *testing.T) {
	_, err := Load(write(t, `
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
  example-feed:
    collector: feed
    cadance: daily
`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cadance")
}

func TestLoadMissingAndEmptyFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open config")

	_, err = Load(write(t, ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestValidationFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"no sources": {
			body: `
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
`,
			want: "at least one entry",
		},
		"unknown collector": {
			body: sourceConfig(`collector: rss
    cadence: daily`),
			want: `unknown collector "rss"`,
		},
		"missing collector": {
			body: sourceConfig(`cadence: daily`),
			want: "collector must not be empty",
		},
		"bad cadence": {
			body: sourceConfig(`collector: feed
    cadence: hourly`),
			want: `cadence "hourly" must be one of daily, weekly, monthly`,
		},
		"missing cadence": {
			body: sourceConfig(`collector: feed`),
			want: `cadence "" must be one of`,
		},
		"base_url empty": {
			body: replaceLine(validConfig, "  base_url: https://api.example.com/v1", `  base_url: ""`),
			want: "base_url must not be empty",
		},
		"base_url wrong scheme": {
			body: replaceLine(validConfig, "  base_url: https://api.example.com/v1", "  base_url: ftp://api.example.com/v1"),
			want: `must be http or https, got "ftp"`,
		},
		"base_url no host": {
			body: replaceLine(validConfig, "  base_url: https://api.example.com/v1", "  base_url: https:///v1"),
			want: "has no host",
		},
		"api_key_env empty": {
			body: replaceLine(validConfig, "  api_key_env: ROOZANE_API_KEY", `  api_key_env: ""`),
			want: "api_key_env must not be empty",
		},
		"api_key_env lower-case": {
			body: replaceLine(validConfig, "  api_key_env: ROOZANE_API_KEY", "  api_key_env: roozane_api_key"),
			want: "environment variable NAME",
		},
		"model item empty": {
			body: replaceLine(validConfig, "    item: small", `    item: ""`),
			want: "models.item must not be empty",
		},
		"model digest empty": {
			body: replaceLine(validConfig, "    digest: large", `    digest: ""`),
			want: "models.digest must not be empty",
		},
		"timeout not positive": {
			body: replaceLine(validConfig, "  timeout: 45s", "  timeout: 0s"),
			want: "timeout must be positive",
		},
		"negative item retention": {
			body: replaceLine(validConfig, "  items: 30", "  items: -1"),
			want: "retention.items must be at least 1 day",
		},
		// An explicit zero must not be swallowed by the default: absent means
		// "90 days", zero means "prune the folder being written into".
		"zero item retention": {
			body: replaceLine(validConfig, "  items: 30", "  items: 0"),
			want: "retention.items must be at least 1 day",
		},
		"negative digest retention": {
			body: replaceLine(validConfig, "  digests: 365", "  digests: -5"),
			want: "retention.digests must not be negative",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.body))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestSourceIDConstraints(t *testing.T) {
	for name, tc := range map[string]struct {
		id   string
		want string // empty means the id is accepted
	}{
		"simple":            {id: "hn", want: ""},
		"digits and hyphen": {id: "site-2-updates", want: ""},
		"upper-case":        {id: "HN", want: "must match [a-z0-9-]"},
		"underscore":        {id: "hn_frontpage", want: "must match [a-z0-9-]"},
		"dot":               {id: "hn.front", want: "must match [a-z0-9-]"},
		"slash":             {id: "hn/front", want: "must match [a-z0-9-]"},
		"space":             {id: "hn front", want: "must match [a-z0-9-]"},
		"leading hyphen":    {id: "-hn", want: "leading or trailing hyphen"},
		"trailing hyphen":   {id: "hn-", want: "leading or trailing hyphen"},
		// `--` is the separator between the id and the item key in item
		// filenames, so an id containing one would make that split ambiguous.
		"doubled hyphen": {id: "hn--front", want: "doubled hyphen"},
	} {
		t.Run(name, func(t *testing.T) {
			body := `
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
  "` + tc.id + `": {collector: feed, cadence: daily}
`
			_, err := Load(write(t, body))

			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(write(t, `
aggregator:
  base_url: ""
  api_key_env: ""
  models: {item: "", digest: ""}
  timeout: 0s
sources:
  BadID: {collector: rss, cadence: hourly}
`))

	require.Error(t, err)
	// One edit pass should be able to fix the whole file, so every problem has
	// to be in the message — not just the first one hit.
	for _, want := range []string{
		"base_url must not be empty",
		"api_key_env must not be empty",
		"models.item must not be empty",
		"models.digest must not be empty",
		"timeout must be positive",
		"must match [a-z0-9-]",
		`unknown collector "rss"`,
		`cadence "hourly"`,
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestAPIKeyEnvErrorDoesNotEchoTheValue(t *testing.T) {
	const pasted = "sk-live-do-not-log-this"

	_, err := Load(write(t, replaceLine(validConfig, "  api_key_env: ROOZANE_API_KEY", "  api_key_env: "+pasted)))

	require.Error(t, err)
	// If this fired because a credential was pasted where a variable name
	// belongs, echoing it back would copy the secret into logs and scrollback.
	assert.NotContains(t, err.Error(), pasted)
	assert.Contains(t, err.Error(), "not a credential")
}

func TestAPIKey(t *testing.T) {
	cfg, err := Load(write(t, validConfig))
	require.NoError(t, err)

	_, err = cfg.APIKey()
	require.Error(t, err, "an unset credential must fail rather than silently sending an empty one")
	assert.Contains(t, err.Error(), "ROOZANE_API_KEY")

	t.Setenv("ROOZANE_API_KEY", "the-credential")
	key, err := cfg.APIKey()
	require.NoError(t, err)
	assert.Equal(t, "the-credential", key)
}

func TestDecodeParams(t *testing.T) {
	cfg, err := Load(write(t, validConfig))
	require.NoError(t, err)

	var params struct {
		URL string `yaml:"url"`
	}
	require.NoError(t, cfg.Sources["example-feed"].DecodeParams(&params))
	assert.Equal(t, "https://example.com/feed.xml", params.URL)

	// A source with no params block decodes to nothing without erroring, so a
	// collector that needs none does not special-case its absence.
	params.URL = "untouched"
	require.NoError(t, cfg.Sources["example-site"].DecodeParams(&params))
	assert.Equal(t, "untouched", params.URL)
}

func TestDurationRejectsUnusableValues(t *testing.T) {
	// A bare number is a YAML scalar, so it decodes into the string the parser
	// then rejects for having no unit — the config's ambiguity about the unit is
	// exactly what the error names.
	_, err := Load(write(t, replaceLine(validConfig, "  timeout: 45s", "  timeout: 45")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing unit")

	// A non-scalar cannot be a duration at all.
	_, err = Load(write(t, replaceLine(validConfig, "  timeout: 45s", "  timeout: {seconds: 45}")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duration must be a string")
}

// TestShippedExampleIsValid keeps config.example.yaml honest: it is the first
// thing anyone copies, so it has to survive the same loader as a real config.
//
// It is loaded from a copy at an explicit mode rather than in place. Git tracks
// only the executable bit, so a checkout's permissions come from the cloning
// user's umask — 0644 under the usual 022, 0664 under 002 — and loading the
// working-tree file directly would make this test pass or fail on the ADR-0003
// §8 permission check according to whose machine it ran on. The question here
// is whether the example's CONTENT is valid, so the copy pins the one variable
// that has nothing to do with that.
func TestShippedExampleIsValid(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "config.example.yaml")
	require.NoError(t, os.WriteFile(path, example, 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.NotEmpty(t, cfg.Sources)
	assert.Equal(t, "ROOZANE_API_KEY", cfg.Aggregator.APIKeyEnv)
}

// sourceConfig builds a config whose only source carries the given body, so a
// source-level test changes exactly one thing against a known-good baseline.
func sourceConfig(sourceBody string) string {
	return `
aggregator:
  base_url: https://api.example.com/v1
  api_key_env: ROOZANE_API_KEY
  models: {item: small, digest: large}
sources:
  example-feed:
    ` + sourceBody + "\n"
}

// replaceLine swaps exactly one line of a config so a failure case differs from
// the valid baseline in one place only. It panics if the line is absent, which
// turns a stale test fixture into an immediate failure rather than a test that
// silently asserts against an unmodified config.
func replaceLine(body, old, replacement string) string {
	if !strings.Contains(body, old+"\n") {
		panic("replaceLine: line not found in fixture: " + old)
	}
	return strings.Replace(body, old+"\n", replacement+"\n", 1)
}

// --- sinks and env allow-lists (ADR-0003 §3, §6) ---

// configWith appends extra top-level YAML to the valid baseline.
func configWith(extra string) string { return validConfig + extra }

func TestSinksOptional(t *testing.T) {
	cfg, err := Load(write(t, validConfig))
	require.NoError(t, err)

	// No sinks is a complete pipeline: the aggregator still writes digests to
	// disk, so an absent sink list must not be an error.
	assert.Empty(t, cfg.Sinks)
}

func TestLoadSinks(t *testing.T) {
	cfg, err := Load(write(t, configWith(`
sinks:
  local-file:
    type: file
    params:
      path: /srv/roozane/digest.md
  notify-script:
    command: ["/usr/local/bin/notify", "--quiet"]
    env: [NOTIFY_TOKEN]
`)))
	require.NoError(t, err)

	require.Len(t, cfg.Sinks, 2)
	assert.Equal(t, "file", cfg.Sinks["local-file"].Type)
	assert.Empty(t, cfg.Sinks["local-file"].Command)
	assert.Equal(t, []string{"/usr/local/bin/notify", "--quiet"}, cfg.Sinks["notify-script"].Command)
	assert.Equal(t, []string{"NOTIFY_TOKEN"}, cfg.Sinks["notify-script"].Env)

	var params struct {
		Path string `yaml:"path"`
	}
	require.NoError(t, cfg.Sinks["local-file"].DecodeParams(&params))
	assert.Equal(t, "/srv/roozane/digest.md", params.Path)

	// A sink with no params block decodes to nothing without erroring.
	params.Path = "untouched"
	require.NoError(t, cfg.Sinks["notify-script"].DecodeParams(&params))
	assert.Equal(t, "untouched", params.Path)
}

func TestSinkValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string // empty means the config is accepted
	}{
		"built-in type": {
			body: "\nsinks:\n  a-sink: {type: file}\n",
		},
		"external command": {
			body: "\nsinks:\n  a-sink: {command: [\"/bin/true\"]}\n",
		},
		"neither type nor command": {
			body: "\nsinks:\n  a-sink: {params: {x: 1}}\n",
			want: "set either type (a built-in sink) or command",
		},
		"both type and command": {
			body: "\nsinks:\n  a-sink: {type: file, command: [\"/bin/true\"]}\n",
			want: "set type or command, not both",
		},
		"empty program in command": {
			body: "\nsinks:\n  a-sink: {command: [\"\", \"--flag\"]}\n",
			want: "first element must be the program to run",
		},
		"bad sink id": {
			body: "\nsinks:\n  Bad_Sink: {type: file}\n",
			want: `sink id "Bad_Sink" must match [a-z0-9-]`,
		},
		// The doubled-hyphen rule exists because `--` separates the id from the
		// item key in item FILENAMES. A sink never produces one, so the rule
		// deliberately does not apply here.
		"doubled hyphen is fine for a sink": {
			body: "\nsinks:\n  a--sink: {type: file}\n",
		},
		"unknown sink field": {
			body: "\nsinks:\n  a-sink: {type: file, kind: whatever}\n",
			want: "kind",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, configWith(tc.body)))

			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEnvAllowList(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string // empty means accepted
	}{
		// validConfig ends inside its `sources:` map, so a two-space-indented
		// entry continues it rather than starting a second mapping key.
		"source env accepted": {
			body: "  ok-source: {collector: feed, cadence: daily, env: [A_TOKEN, B_TOKEN]}\n",
		},
		"source env lower-case": {
			body: "  ok-source: {collector: feed, cadence: daily, env: [a_token]}\n",
			want: `source "ok-source": env[0] must be an environment variable NAME`,
		},
		"source env empty entry": {
			body: "  ok-source: {collector: feed, cadence: daily, env: [\"\"]}\n",
			want: `source "ok-source": env[0] must not be empty`,
		},
		"source env duplicate": {
			body: "  ok-source: {collector: feed, cadence: daily, env: [A_TOKEN, A_TOKEN]}\n",
			want: `source "ok-source": env lists A_TOKEN twice`,
		},
		"second env entry reports its own index": {
			body: "  ok-source: {collector: feed, cadence: daily, env: [A_TOKEN, b_token]}\n",
			want: `source "ok-source": env[1] must be an environment variable NAME`,
		},
		"sink env lower-case": {
			body: "\nsinks:\n  a-sink: {type: file, env: [a_token]}\n",
			want: `sink "a-sink": env[0] must be an environment variable NAME`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, configWith(tc.body)))

			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEnvErrorDoesNotEchoTheValue(t *testing.T) {
	const pasted = "sk-live-do-not-log-this"

	_, err := Load(write(t, configWith("\nsinks:\n  a-sink: {type: file, env: [\""+pasted+"\"]}\n")))

	require.Error(t, err)
	// Same reasoning as the aggregator credential: an env allow-list is where a
	// secret gets pasted by mistake, so the error must not repeat it.
	assert.NotContains(t, err.Error(), pasted)
	assert.Contains(t, err.Error(), "not a credential")
}

func TestCadenceDays(t *testing.T) {
	for _, tc := range []struct {
		cadence Cadence
		want    int
	}{
		{CadenceDaily, 1},
		{CadenceWeekly, 7},
		{CadenceMonthly, 30},
	} {
		days, ok := tc.cadence.Days()
		require.True(t, ok, "%q is part of the vocabulary", tc.cadence)
		assert.Equal(t, tc.want, days, "cadence %q", tc.cadence)
	}

	// An unknown cadence must not read as zero days: a caller that took that at
	// face value would treat the source as due on every pass.
	days, ok := Cadence("fortnightly").Days()
	assert.False(t, ok)
	assert.Zero(t, days)
}

// retentionConfig is validConfig with the item window and the second source's
// cadence swapped in, so a test changes only what it is about.
func retentionConfig(items int, cadence Cadence) string {
	body := strings.Replace(validConfig, "  items: 30", "  items: "+strconv.Itoa(items), 1)
	return strings.Replace(body, "    cadence: weekly", "    cadence: "+string(cadence), 1)
}

// TestRetentionShorterThanLongestCadenceIsRejected is the rule itself: a window
// that would prune a source's evidence before it is due again must fail at
// validation rather than degrade quietly at run time.
func TestRetentionShorterThanLongestCadenceIsRejected(t *testing.T) {
	// Vacuity guard: the same config with an adequate window loads cleanly, so
	// the failure below is provably about the window and not about the fixture.
	_, err := Load(write(t, retentionConfig(30, CadenceMonthly)))
	require.NoError(t, err, "the fixture must be valid apart from the window under test")

	_, err = Load(write(t, retentionConfig(7, CadenceMonthly)))
	require.Error(t, err, "a 7-day window with a monthly source must be rejected")

	// The message has to be actionable: which window, which source, which
	// cadence, and how long it needs to be.
	msg := err.Error()
	assert.Contains(t, msg, "retention.items is 7 days")
	assert.Contains(t, msg, `"example-site"`)
	assert.Contains(t, msg, "monthly")
	assert.Contains(t, msg, "30 days")
}

// TestRetentionEqualToLongestCadenceIsAccepted pins the boundary. Equality is
// sufficient: a source whose last run falls on the oldest kept day is already
// due by then, so only the days strictly inside the period matter.
func TestRetentionEqualToLongestCadenceIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		items   int
		cadence Cadence
	}{
		{"weekly at exactly seven", 7, CadenceWeekly},
		{"monthly at exactly thirty", 30, CadenceMonthly},
		{"daily at exactly one", 1, CadenceDaily},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, retentionConfig(tc.items, tc.cadence)))
			assert.NoError(t, err, "a window equal to the longest cadence is enough")
		})
	}
}

// TestRetentionOneDayShortOfLongestCadenceIsRejected is the other side of the
// same boundary — without it, a rule that only fired far below the threshold
// would still pass the equality test above.
func TestRetentionOneDayShortOfLongestCadenceIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		items   int
		cadence Cadence
	}{
		{"weekly at six", 6, CadenceWeekly},
		{"monthly at twenty-nine", 29, CadenceMonthly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, retentionConfig(tc.items, tc.cadence)))
			assert.Error(t, err, "one day short of the cadence still prunes the deciding day")
		})
	}
}

// TestRetentionIsMeasuredAgainstTheLongestCadenceNotTheFirst guards against a
// rule that stopped at whichever source it happened to read first.
func TestRetentionIsMeasuredAgainstTheLongestCadenceNotTheFirst(t *testing.T) {
	// example-feed is daily and sorts first; example-site is monthly. A window
	// of 7 satisfies the daily source and must still be rejected for the other.
	_, err := Load(write(t, retentionConfig(7, CadenceMonthly)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"example-site"`,
		"the rule must report the source that actually needs the longer window")
}

// TestRetentionRuleIgnoresAnUnknownCadence keeps one mistake to one complaint:
// an unrecognised cadence is already a validation error on its own terms.
func TestRetentionRuleIgnoresAnUnknownCadence(t *testing.T) {
	_, err := Load(write(t, retentionConfig(1, "fortnightly")))
	require.Error(t, err, "the unknown cadence itself is still an error")

	msg := err.Error()
	assert.Contains(t, msg, "cadence")
	assert.NotContains(t, msg, "retention.items is 1 days",
		"an unknown cadence has no length, so the retention rule must stay silent about it")
}

func TestExecSourceRequiresACommand(t *testing.T) {
	// Vacuity guard: the same source WITH a command loads cleanly, so the
	// failure below is about the missing command and not the fixture.
	_, err := Load(write(t, sourceConfig("collector: exec\n    cadence: daily\n    command: [/bin/true]")))
	require.NoError(t, err)

	_, err = Load(write(t, sourceConfig("collector: exec\n    cadence: daily")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a command")
}

// TestCommandOnANonExecCollectorIsRejected — a command that would be silently
// ignored is worse than being told about it.
func TestCommandOnANonExecCollectorIsRejected(t *testing.T) {
	_, err := Load(write(t, sourceConfig("collector: http\n    cadence: daily\n    command: [/bin/true]\n    params:\n      url: https://example.com/")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only meaningful for the \"exec\" collector")
}

func TestExecIsAKnownCollector(t *testing.T) {
	cfg, err := Load(write(t, sourceConfig("collector: exec\n    cadence: daily\n    command: [/bin/true]")))
	require.NoError(t, err)
	assert.Equal(t, "exec", cfg.Sources["example-feed"].Collector)
	assert.Equal(t, []string{"/bin/true"}, cfg.Sources["example-feed"].Command)
}
