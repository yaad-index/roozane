// Package config defines and loads Roozane's YAML configuration.
//
// The configuration is the product surface: ADR-0001 requires that adding a
// source is a config change and never a code change, and that the reader owns
// the relevance profile rather than inferring it. Everything here follows from
// that — the source list is open-ended, the aggregator is described by endpoint
// and model names rather than by any particular provider, and credentials are
// referenced by environment-variable name so a config file can be committed,
// diffed and shared without carrying a secret.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// Config is a whole roozane.yaml.
type Config struct {
	// DataRoot is the single root under which everything on disk lives
	// (ADR-0002). Relative paths resolve against the config file's directory,
	// so a config and its data can move together.
	DataRoot string `yaml:"data_root"`

	// RelevanceProfile points at the reader-owned description of what matters
	// to them. It is prompt input for the aggregator, never inferred state.
	RelevanceProfile string `yaml:"relevance_profile"`

	Retention  Retention  `yaml:"retention"`
	Aggregator Aggregator `yaml:"aggregator"`

	// Sources is keyed by source id. The key is not decoration: ADR-0002 makes
	// it a path component of every collected item's filename, which is why
	// validateSourceID is strict about it.
	Sources map[string]Source `yaml:"sources"`

	// Sinks is keyed by sink id and is optional: with none configured the
	// aggregator still writes digests to disk, which is a complete pipeline for
	// a reader who only wants the files.
	Sinks map[string]Sink `yaml:"sinks"`

	// dir is the directory the config was loaded from, used to resolve
	// relative paths. Unexported so it cannot be set from the file itself.
	dir string
}

// Retention is the two independent windows ADR-0002 defines, both counted in
// UTC days.
type Retention struct {
	// Items prunes day folders under days/. It is a pointer so that an absent
	// window and an explicit `items: 0` stay distinguishable: the first takes
	// the default, the second is a request to delete the day currently being
	// written, which Validate rejects. Load always leaves this non-nil.
	Items *int `yaml:"items"`

	// Digests prunes the digests/ tree. Zero means keep forever, which is also
	// the default, so absent and explicit zero mean the same thing here and no
	// pointer is needed.
	Digests int `yaml:"digests"`
}

// Aggregator describes the one layer with a brain (ADR-0001 §3). It is
// deliberately expressed as "an endpoint speaking the standard
// chat-completions shape, plus model names" so that no provider is named,
// assumed, or endorsed anywhere in the engine.
type Aggregator struct {
	// BaseURL is the chat-completions endpoint root.
	BaseURL string `yaml:"base_url"`

	// APIKeyEnv names the environment variable holding the credential — it is
	// a reference, never the credential itself. validateAggregator enforces
	// the shape of a variable name, so a pasted secret fails loudly at load
	// rather than silently becoming a "variable name" that never resolves.
	APIKeyEnv string `yaml:"api_key_env"`

	Models Models `yaml:"models"`

	// Timeout is a pointer for the same reason Retention.Items is: an absent
	// timeout should take the default, while an explicit `timeout: 0s` is a
	// request for no timeout at all and must be rejected rather than silently
	// becoming 60s. Load always leaves this non-nil.
	Timeout *Duration `yaml:"timeout"`
}

// Models is the small-per-item / larger-for-the-digest split ADR-0001 calls
// for, so cost scales with the size of the task.
type Models struct {
	Item   string `yaml:"item"`
	Digest string `yaml:"digest"`
}

// Source is one entry in the source list: what to run, how often, and whatever
// that collector needs.
type Source struct {
	Collector string  `yaml:"collector"`
	Cadence   Cadence `yaml:"cadence"`

	// Env is the allow-list of environment variables this entry's collector
	// receives (ADR-0003 §6). It names variables; it never carries values.
	Env []string `yaml:"env"`

	// Params is held undecoded so each collector can decode its own settings
	// with its own strictness. Keeping it opaque here is what lets a new
	// collector — eventually an external one, per ADR-0001 §5 — arrive without
	// this package learning about it.
	Params yaml.Node `yaml:"params"`
}

// DecodeParams decodes a source's collector-specific params into v. Sources
// with no params block decode to nothing and report no error, so a collector
// that needs none does not have to special-case its absence.
func (s Source) DecodeParams(v any) error { return decodeParams(s.Params, v) }

// Sink is one entry in the sink list: where a digest goes and what that
// delivery needs (ADR-0003 §3). Exactly one of Type and Command identifies it —
// a built-in delivery or an external program — which is what keeps the edge
// open without making "which one runs" ambiguous.
type Sink struct {
	// Type names a built-in sink. The set of built-in types is defined by the
	// sink layer itself, so this package deliberately does not police the value
	// beyond requiring one: guessing an allow-list here would pre-empt that
	// work and invent names nothing implements. The trade-off is real and
	// stated rather than hidden — a misspelled type is caught when the sink
	// layer resolves it, not at load.
	Type string `yaml:"type"`

	// Command is an exec-based sink: the program and its arguments, invoked
	// under the ADR-0003 contract.
	Command []string `yaml:"command"`

	// Env is the allow-list of environment variables this sink receives — for
	// a delivery credential, by variable name (ADR-0003 §6).
	Env []string `yaml:"env"`

	// Params carries sink-specific settings — a destination id, a path — and is
	// held undecoded for the same reason a source's params are.
	Params yaml.Node `yaml:"params"`
}

// DecodeParams decodes a sink's delivery-specific params into v, with the same
// absent-is-not-an-error behaviour as Source.DecodeParams.
func (s Sink) DecodeParams(v any) error { return decodeParams(s.Params, v) }

func decodeParams(node yaml.Node, v any) error {
	if node.IsZero() {
		return nil
	}
	return node.Decode(v)
}

// Cadence is how often a source is fetched. ADR-0001 fixes the vocabulary at
// three values rather than free-form intervals: the point is a config a person
// reads at a glance, not a scheduler language.
type Cadence string

const (
	CadenceDaily   Cadence = "daily"
	CadenceWeekly  Cadence = "weekly"
	CadenceMonthly Cadence = "monthly"
)

var cadences = []Cadence{CadenceDaily, CadenceWeekly, CadenceMonthly}

// cadenceDays is how many UTC days must pass before a source is due again.
// Monthly is 30 days rather than calendar-month arithmetic: for a fetch
// schedule, "every 30 days" is predictable and "the 31st of the month" is a bug
// report waiting to happen.
var cadenceDays = map[Cadence]int{
	CadenceDaily:   1,
	CadenceWeekly:  7,
	CadenceMonthly: 30,
}

// Valid reports whether c is one of the three defined cadences.
func (c Cadence) Valid() bool {
	for _, known := range cadences {
		if c == known {
			return true
		}
	}
	return false
}

// Days is how many UTC days must pass before a source on this cadence is due
// again. The second result is false for a cadence outside the vocabulary, so a
// caller cannot silently read an unknown cadence as zero days and fetch on
// every pass.
//
// This lives with the Cadence type rather than with the collector because two
// packages now need it: the collector to decide due-ness, and validation to
// check the retention window is long enough to keep a cadence's evidence.
func (c Cadence) Days() (int, bool) {
	days, ok := cadenceDays[c]
	return days, ok
}

// Duration is a time.Duration that reads from YAML as a human string ("45s",
// "2m"), because a bare number in a config file is ambiguous about its unit.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"60s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// builtinCollectors are the collector types this package knows. Anything else
// is rejected rather than passed through: an unknown type here is far more
// likely to be a typo than a plugin, and the exec-plugin contract that would
// legitimately widen this set is still a later ADR.
var builtinCollectors = []string{"feed", "http"}

// Defaults applied when a field is absent. They are the values that make an
// otherwise-minimal config runnable, not policy.
const (
	defaultDataRoot         = "data"
	defaultRelevanceProfile = "profile.md"
	defaultRetentionItems   = 90
	defaultTimeout          = 60 * time.Second
)

// idPattern is ADR-0002's `[a-z0-9-]` constraint, anchored, and refusing a
// leading or trailing hyphen. Shared by source and sink ids.
var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// envVarPattern is the conventional shape of an environment variable name. It
// doubles as the guard that keeps an actual credential out of the file: a real
// key contains characters this rejects.
var envVarPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Load reads, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file; a close error says nothing actionable

	cfg, err := parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	cfg.dir = dir
	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// parse decodes YAML strictly: an unknown key is an error, not a shrug. A
// misspelled field in a config whose whole job is to be edited by hand would
// otherwise be silently ignored, and the reader would be left wondering why
// their change did nothing.
func parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.DataRoot == "" {
		c.DataRoot = defaultDataRoot
	}
	if c.RelevanceProfile == "" {
		c.RelevanceProfile = defaultRelevanceProfile
	}
	if c.Retention.Items == nil {
		items := defaultRetentionItems
		c.Retention.Items = &items
	}
	if c.Aggregator.Timeout == nil {
		timeout := Duration(defaultTimeout)
		c.Aggregator.Timeout = &timeout
	}
}

// ItemDays is the item-retention window in UTC days. Load guarantees the
// underlying value is set; the nil branch keeps a hand-built Config from
// panicking.
func (r Retention) ItemDays() int {
	if r.Items == nil {
		return defaultRetentionItems
	}
	return *r.Items
}

// RequestTimeout is the per-request timeout for aggregator calls, with the same
// nil-safety as Retention.ItemDays.
func (a Aggregator) RequestTimeout() time.Duration {
	if a.Timeout == nil {
		return defaultTimeout
	}
	return a.Timeout.Duration()
}

// Validate reports every problem it finds rather than the first, so one edit
// pass can fix a broken config instead of one round trip per mistake.
func (c *Config) Validate() error {
	var problems []error

	if c.DataRoot == "" {
		problems = append(problems, errors.New("data_root must not be empty"))
	}
	if c.RelevanceProfile == "" {
		problems = append(problems, errors.New("relevance_profile must not be empty"))
	}
	if items := c.Retention.ItemDays(); items < 1 {
		// Zero is rejected rather than treated as "keep nothing": a window of
		// zero days would prune the day folder collectors are still writing
		// into. Someone who wants no history keeps one day.
		problems = append(problems, fmt.Errorf("retention.items must be at least 1 day, got %d", items))
	}
	if c.Retention.Digests < 0 {
		problems = append(problems, fmt.Errorf("retention.digests must not be negative, got %d (0 means keep forever)", c.Retention.Digests))
	}

	problems = append(problems, c.validateAggregator()...)
	problems = append(problems, c.validateSources()...)
	problems = append(problems, c.validateRetentionCoversCadences()...)
	problems = append(problems, c.validateSinks()...)

	return errors.Join(problems...)
}

func (c *Config) validateAggregator() []error {
	var problems []error
	a := c.Aggregator

	if a.BaseURL == "" {
		problems = append(problems, errors.New("aggregator.base_url must not be empty"))
	} else {
		u, err := url.Parse(a.BaseURL)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("aggregator.base_url is not a valid URL: %w", err))
		case u.Scheme != "http" && u.Scheme != "https":
			problems = append(problems, fmt.Errorf("aggregator.base_url must be http or https, got %q", u.Scheme))
		case u.Host == "":
			problems = append(problems, fmt.Errorf("aggregator.base_url has no host: %q", a.BaseURL))
		}
	}

	switch {
	case a.APIKeyEnv == "":
		problems = append(problems, errors.New("aggregator.api_key_env must not be empty: it names the environment variable holding the credential"))
	case !envVarPattern.MatchString(a.APIKeyEnv):
		// Deliberately does not echo the value: if this fired because someone
		// pasted the credential itself, repeating it back would write the
		// secret into logs and terminal scrollback.
		problems = append(problems, errors.New("aggregator.api_key_env must be an environment variable NAME (upper-case letters, digits and underscores), not a credential"))
	}

	if a.Models.Item == "" {
		problems = append(problems, errors.New("aggregator.models.item must not be empty"))
	}
	if a.Models.Digest == "" {
		problems = append(problems, errors.New("aggregator.models.digest must not be empty"))
	}
	if timeout := a.RequestTimeout(); timeout <= 0 {
		problems = append(problems, fmt.Errorf("aggregator.timeout must be positive, got %s", timeout))
	}

	return problems
}

func (c *Config) validateSources() []error {
	if len(c.Sources) == 0 {
		return []error{errors.New("sources must contain at least one entry: a pipeline with no sources can only ever produce an empty digest")}
	}

	var problems []error
	for _, id := range sortedKeys(c.Sources) {
		if err := validateSourceID(id); err != nil {
			problems = append(problems, err)
		}
		problems = append(problems, validateSource(id, c.Sources[id])...)
	}
	return problems
}

// validateRetentionCoversCadences rejects an item-retention window shorter than
// the longest configured cadence.
//
// The collector derives a source's last run from the layout, looking back one
// cadence period. Item retention prunes whole day folders, so a window shorter
// than that period deletes the evidence — items and empty-run markers alike —
// before the source is due again, and the source silently returns to being
// fetched on every pass. That is the behaviour ADR-0004 removes, reintroduced
// through a retention setting rather than a collector bug, which is why it is
// worth failing loudly here instead of degrading quietly at run time.
//
// Equality passes, and is exactly sufficient rather than generous: a source
// whose last run falls on the oldest day the window keeps is already due by
// then, so pruning that day changes nothing. Only the days strictly inside the
// period distinguish "not due" from "due", and a window equal to the period
// keeps all of them.
func (c *Config) validateRetentionCoversCadences() []error {
	longestDays, longestSource := 0, ""
	for _, id := range sortedKeys(c.Sources) {
		// An unknown cadence is already reported by validateSources; counting
		// it here would be a second complaint about one mistake.
		days, ok := c.Sources[id].Cadence.Days()
		if !ok {
			continue
		}
		if days > longestDays {
			longestDays, longestSource = days, id
		}
	}
	if longestDays == 0 {
		return nil
	}

	items := c.Retention.ItemDays()
	if items >= longestDays {
		return nil
	}
	return []error{fmt.Errorf(
		"retention.items is %d days but source %q has cadence %q (%d days): "+
			"the window must be at least as long as the longest cadence, or that source's "+
			"items are pruned before it is due again and it is re-fetched on every pass",
		items, longestSource, c.Sources[longestSource].Cadence, longestDays)}
}

// validateSinks checks the optional sink list. An empty one is fine: without
// sinks the aggregator still writes digests to disk.
func (c *Config) validateSinks() []error {
	var problems []error
	for _, id := range sortedKeys(c.Sinks) {
		if err := validateID("sink", id); err != nil {
			problems = append(problems, err)
		}
		problems = append(problems, validateSink(id, c.Sinks[id])...)
	}
	return problems
}

// sortedKeys returns a map's keys in a stable order. Map iteration is random,
// and a validator that reports the same broken config in a different order each
// run is harder to work through than one that does not.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateID is the id shape shared by sources and sinks: lower-case letters,
// digits and interior hyphens. Both kinds end up in paths and log lines, so a
// single rule is easier to remember than two nearly-identical ones.
func validateID(kind, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%s id %q must match [a-z0-9-], without a leading or trailing hyphen", kind, id)
	}
	return nil
}

func validateSourceID(id string) error {
	if err := validateID("source", id); err != nil {
		// A source id carries the extra constraint below, but reporting both at
		// once would be noise when the id is malformed to begin with.
		return fmt.Errorf("%w: it becomes a path component of every item file (ADR-0002)", err)
	}
	// ADR-0002 names item files `<source-id>--<key>.md`. A doubled hyphen
	// inside the id would make that split ambiguous, so it is rejected here
	// rather than producing files nothing can reliably parse back. This applies
	// to sources only — a sink id never becomes an item filename.
	if strings.Contains(id, "--") {
		return fmt.Errorf("source id %q must not contain a doubled hyphen: `--` separates the id from the item key in item filenames (ADR-0002)", id)
	}
	return nil
}

func validateSource(id string, s Source) []error {
	var problems []error

	switch {
	case s.Collector == "":
		problems = append(problems, fmt.Errorf("source %q: collector must not be empty", id))
	case !isBuiltinCollector(s.Collector):
		problems = append(problems, fmt.Errorf("source %q: unknown collector %q, want one of %s", id, s.Collector, strings.Join(builtinCollectors, ", ")))
	}

	if !s.Cadence.Valid() {
		want := make([]string, len(cadences))
		for i, c := range cadences {
			want[i] = string(c)
		}
		problems = append(problems, fmt.Errorf("source %q: cadence %q must be one of %s", id, s.Cadence, strings.Join(want, ", ")))
	}

	problems = append(problems, validateEnv(fmt.Sprintf("source %q", id), s.Env)...)

	return problems
}

func validateSink(id string, s Sink) []error {
	var problems []error

	// Exactly one of the two. Neither leaves nothing to run; both leaves the
	// sink layer to pick, and a config that needs a tie-break rule is a config
	// whose author did not say what they meant.
	switch {
	case s.Type == "" && len(s.Command) == 0:
		problems = append(problems, fmt.Errorf("sink %q: set either type (a built-in sink) or command (an external one)", id))
	case s.Type != "" && len(s.Command) > 0:
		problems = append(problems, fmt.Errorf("sink %q: set type or command, not both", id))
	case len(s.Command) > 0 && s.Command[0] == "":
		problems = append(problems, fmt.Errorf("sink %q: command's first element must be the program to run, not an empty string", id))
	}

	problems = append(problems, validateEnv(fmt.Sprintf("sink %q", id), s.Env)...)

	return problems
}

// validateEnv checks an ADR-0003 §6 allow-list. Every entry must look like a
// variable name for the same reason aggregator.api_key_env must: the config
// carries names, and a pasted secret has to fail here rather than becoming a
// name that never resolves.
func validateEnv(who string, env []string) []error {
	var problems []error
	seen := make(map[string]bool, len(env))

	for i, name := range env {
		switch {
		case name == "":
			problems = append(problems, fmt.Errorf("%s: env[%d] must not be empty", who, i))
		case !envVarPattern.MatchString(name):
			// Deliberately does not echo the value — same reasoning as
			// validateAggregator: it may be the credential itself.
			problems = append(problems, fmt.Errorf("%s: env[%d] must be an environment variable NAME (upper-case letters, digits and underscores), not a credential", who, i))
		case seen[name]:
			problems = append(problems, fmt.Errorf("%s: env lists %s twice", who, name))
		default:
			seen[name] = true
		}
	}

	return problems
}

func isBuiltinCollector(name string) bool {
	for _, known := range builtinCollectors {
		if name == known {
			return true
		}
	}
	return false
}

// DataRootPath is the absolute data root, resolving a relative data_root
// against the config file's own directory.
func (c *Config) DataRootPath() string { return c.resolve(c.DataRoot) }

// RelevanceProfilePath is the absolute path to the relevance profile, resolved
// the same way as the data root.
func (c *Config) RelevanceProfilePath() string { return c.resolve(c.RelevanceProfile) }

func (c *Config) resolve(path string) string {
	if filepath.IsAbs(path) || c.dir == "" {
		return path
	}
	return filepath.Join(c.dir, path)
}

// APIKey reads the aggregator credential from the environment variable named
// by api_key_env. The credential lives only in the environment: it is never
// read from, written to, or defaulted into the config file.
func (c *Config) APIKey() (string, error) {
	key := os.Getenv(c.Aggregator.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("environment variable %s is unset or empty: it must hold the aggregator credential", c.Aggregator.APIKeyEnv)
	}
	return key, nil
}
