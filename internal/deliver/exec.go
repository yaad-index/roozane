package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// contractVersion is the ADR-0003 plugin contract this engine speaks. A plugin
// reads it and fails loudly rather than misparsing a shape it does not know.
const contractVersion = 1

// envelope is the ADR-0003 §3 sink envelope. The header sits alongside params
// rather than merged into them, so a plugin whose params happen to include
// `sink` or `contract` cannot collide with it.
type envelope struct {
	Contract int             `json:"contract"`
	Sink     string          `json:"sink"`
	Params   map[string]any  `json:"params,omitempty"`
	Digest   json.RawMessage `json:"digest"`
}

// waitDelay bounds how long Wait may go on reading a plugin's pipes after the
// context ends or the process exits. Without it, Wait reads to EOF — and EOF
// does not arrive while ANY descendant still holds the write end, including one
// that called setsid and so sits outside the process group the timeout kills.
// The group kill then succeeds and the pass hangs regardless, defeating the
// invariant the kill exists for.
//
// Five seconds is far more than a dead process's buffered output needs to
// drain, and the delay only starts once the process has exited or the deadline
// has passed, so it costs a healthy plugin nothing.
const waitDelay = 5 * time.Second

// maxPluginOutput bounds what is read back from a plugin's stdout for logging.
const maxPluginOutput = 64 * 1024

// execSink runs an external program under the ADR-0003 contract.
type execSink struct {
	id      string
	command []string
	params  yaml.Node
	env     []string
}

// Deliver invokes the plugin with the envelope on stdin. Exit code is the whole
// success signal (ADR-0003 §4); stdout is logged, stderr is captured for the
// failure message.
func (s *execSink) Deliver(ctx context.Context, digest Digest) error {
	if len(s.command) == 0 {
		return errors.New("sink has no command")
	}

	params, err := s.decodedParams()
	if err != nil {
		return err
	}

	structured := digest.Structured
	if len(structured) == 0 {
		structured = []byte("null")
	}
	payload, err := json.Marshal(envelope{
		Contract: contractVersion,
		Sink:     s.id,
		Params:   params,
		Digest:   json.RawMessage(structured),
	})
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}

	cmd := exec.CommandContext(ctx, s.command[0], s.command[1:]...) //nolint:gosec // the command is what the operator configured; ADR-0003 §8 makes the config the trust boundary
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = s.environment()

	// Give the plugin its own process group, created here precisely so that
	// killing it on timeout can never reach the engine itself (ADR-0003 §5).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole group, so a plugin that spawned
		// children does not leave them behind.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Cap how long Wait may keep reading the pipes after the deadline, so a
	// descendant outside the killed group cannot hold the delivery open.
	cmd.WaitDelay = waitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	if out := strings.TrimSpace(stdout.String()); out != "" {
		// Anything the sink prints is logged verbatim (ADR-0003 §3), bounded so
		// a chatty plugin cannot flood the log.
		fmt.Fprintln(os.Stderr, "sink "+s.id+": "+truncate(out, maxPluginOutput))
	}

	if runErr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", runErr, truncate(msg, 2048))
		}
		return runErr
	}
	return nil
}

// environment is the minimal environment of ADR-0003 §6: PATH, locale, and only
// the variables this sink's config names. Nothing else leaks through — a plugin
// gets what it was granted, not whatever the engine happened to be started with.
func (s *execSink) environment() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LANG=" + os.Getenv("LANG"),
		"LC_ALL=" + os.Getenv("LC_ALL"),
	}
	for _, name := range s.env {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// decodedParams turns the sink's YAML params into plain values for the JSON
// envelope. Absent params send nothing rather than an empty object.
func (s *execSink) decodedParams() (map[string]any, error) {
	if s.params.IsZero() {
		return nil, nil
	}
	var params map[string]any
	if err := s.params.Decode(&params); err != nil {
		return nil, fmt.Errorf("sink params: %w", err)
	}
	return params, nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
