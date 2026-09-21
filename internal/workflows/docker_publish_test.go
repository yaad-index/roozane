// Package workflows holds no code of its own. It exists for assertions about
// this repository's GitHub Actions workflow files, which nothing else in the
// build can reach: a workflow is YAML that only ever executes in CI, so a
// change to one compiles, vets, lints and passes every other test while being
// wrong, and is discovered on the merge it fails to guard.
package workflows

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"
)

// ciRunsQuery finds the query string of the gate's call to the workflow-runs
// API.
//
// ⚠️ It matches the URL rather than searching the script for "event=push",
// which a comment mentioning the parameter would also satisfy. The assertion
// has to be about the request that will actually be sent.
var ciRunsQuery = regexp.MustCompile(`/actions/workflows/ci\.yml/runs\?([^"'\s]+)`)

// TestThePublishGateAcceptsOnlyPushRuns pins the filter the gate's correctness
// rests on.
//
// 🚨 Without `event=push` the gate is correct only because of a property it does
// not control: squash merging produces a SHA that no `pull_request` run ever
// carried, so the only run that can match is the push run on main. Switch the
// repository to merge commits and a commit on main can carry the PR head's SHA
// — at which point a green PR run, which validated the head against the base as
// it stood then rather than the result, satisfies the gate for a commit whose
// push run failed. That is the hole #4 exists to close, reopened by a setting
// rather than by code.
//
// It is latent, not live: under the repository's current merge strategy the
// filter changes no outcome. It is pinned here because the thing that would
// make it live is a change in a settings page, where no test runs and no
// reviewer is looking at this file.
func TestThePublishGateAcceptsOnlyPushRuns(t *testing.T) {
	script := gateScript(t)

	match := ciRunsQuery.FindStringSubmatch(script)
	require.Len(t, match, 2,
		"the gate must call the workflow-runs API for ci.yml; without that call there is nothing to filter")

	// Parsed rather than string-matched, so the assertion is about the
	// parameters the request carries and not about the spelling of the line.
	query, err := url.ParseQuery(match[1])
	require.NoError(t, err, "the gate's query string must parse: %q", match[1])

	assert.Equal(t, []string{"push"}, query["event"],
		"the gate must accept only push-triggered CI runs; without this a green pull_request run can satisfy it for a commit whose push run failed, the moment the repository stops squashing")
	assert.NotEmpty(t, query["head_sha"],
		"the gate must ask about one commit; an unfiltered query would let any recent green run satisfy it")
}

// gateScript returns the shell of the step that waits for CI, read out of the
// workflow itself so the test cannot drift from what runs.
func gateScript(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "docker-publish.yml"))
	require.NoError(t, err)

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &workflow))

	job, ok := workflow.Jobs["require-passing-suite"]
	require.True(t, ok,
		"docker-publish must have a require-passing-suite job; if it was renamed, this test is looking at nothing and would pass for the wrong reason")

	for _, step := range job.Steps {
		if step.Run != "" {
			return step.Run
		}
	}

	require.FailNow(t, "the require-passing-suite job has no run: step to inspect")
	return ""
}
