package config

import (
	"os"
	"path/filepath"
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
func TestShippedExampleIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
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
