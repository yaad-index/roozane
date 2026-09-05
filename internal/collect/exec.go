package collect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/yaad-index/roozane/internal/config"
)

// contractVersion is the ADR-0003 plugin contract this engine speaks. It goes
// in the envelope so a plugin that needs a newer contract fails loudly instead
// of misparsing a shape it does not know (§7).
const contractVersion = 1

// execTimeout bounds one plugin run. ADR-0003 §5 wants this configurable per
// source; the sink side shipped with a package constant for the same reason
// this does — making it a config field is its own change, in both places at
// once, rather than half here.
const execTimeout = 60 * time.Second

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

// maxPluginStderr bounds what is quoted back from a failing plugin, so a
// chatty failure cannot flood the log through an error message.
const maxPluginStderr = 2048

// maxLineBytes bounds one NDJSON line. It is deliberately far larger than
// maxItemBytes: the item cap applies to an item's CONTENT once parsed, while a
// line also carries JSON field names and escaping, and escaping can inflate
// content several times over. Sized so a legitimately max-size item always fits
// with room to spare; a line beyond it is skipped and logged.
const maxLineBytes = 8 * maxItemBytes

// maxPluginOutput bounds the plugin's whole stdout stream. Items are capped
// individually by capContent, but a plugin that never stops writing would
// otherwise be read until the engine ran out of memory — the cap is on the
// stream as well as on what comes out of it.
const maxPluginOutput = 64 << 20

// envelope is the ADR-0003 §2 collector envelope. The header sits alongside
// params rather than merged into them, so a plugin whose params happen to use
// `source` or `contract` cannot collide with it.
type envelope struct {
	Contract int            `json:"contract"`
	Source   string         `json:"source"`
	Params   map[string]any `json:"params,omitempty"`
}

// pluginItem is one NDJSON line from a collector plugin. Only content is
// required; the rest is provenance.
//
// There is deliberately no fetched_at: ADR-0003 §2 has the engine stamp it with
// its own clock, always. A plugin-supplied time is preserved as source_time and
// never chooses the day key, which is what stops a plugin writing into an
// immutable past day.
type pluginItem struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	SourceTime string `json:"source_time"`
	Content    string `json:"content"`
}

// execCollector runs an external program under the ADR-0003 contract.
type execCollector struct {
	log *slog.Logger
}

// Collect invokes the plugin with the envelope on stdin and parses NDJSON items
// off its stdout.
func (c *execCollector) Collect(ctx context.Context, id string, src config.Source) ([]Collected, error) {
	if len(src.Command) == 0 {
		// Config validation requires a command for this collector, so reaching
		// here means the schema and this code have drifted apart.
		return nil, errors.New("exec source has no command")
	}

	var params map[string]any
	if err := src.DecodeParams(&params); err != nil {
		return nil, fmt.Errorf("source params: %w", err)
	}

	payload, err := json.Marshal(envelope{Contract: contractVersion, Source: id, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode envelope: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, src.Command[0], src.Command[1:]...) //nolint:gosec // the command is what the operator configured; ADR-0003 §8 makes the config the trust boundary
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = environment(src.Env)

	// Give the plugin its own process group, created here precisely so that
	// killing it on timeout can never reach the engine itself (ADR-0003 §5).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole group, so a plugin that spawned
		// children does not leave them behind holding the day's run hostage.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Cap how long Wait may keep reading the pipes after the deadline, so a
	// descendant outside the killed group cannot hold the pass open.
	cmd.WaitDelay = waitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// Parse whatever arrived before deciding on the error. ADR-0003 §4 keeps
	// items a plugin emitted before failing — they were valid when parsed — so
	// the items are read out even on a non-zero exit.
	items := c.parse(id, &stdout)

	if runErr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return items, fmt.Errorf("%w: %s", runErr, truncate(msg, maxPluginStderr))
		}
		return items, runErr
	}
	return items, nil
}

// parse reads NDJSON items off the plugin's stdout. A malformed line is logged
// and skipped rather than fatal (ADR-0003 §2): one bad item must not cost the
// whole batch.
func (c *execCollector) parse(id string, r io.Reader) []Collected {
	var items []Collected

	reader := bufio.NewReaderSize(io.LimitReader(r, maxPluginOutput), 64*1024)

	for line := 1; ; line++ {
		raw, tooLong, err := readLine(reader, maxLineBytes)
		if tooLong {
			// Skipped rather than truncated: the engine caps an item's CONTENT
			// after parsing, but a JSON line this long cannot be parsed at all
			// without buffering it whole, so there is nothing to take a prefix
			// of. Reading on is what keeps it one lost item and not the batch.
			c.log.Warn("skipping oversize plugin line", "source", id, "line", line, "limit_bytes", maxLineBytes)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.log.Error("stopped reading plugin output", "source", id, "error", err)
			}
			break
		}
		if tooLong {
			continue
		}

		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}

		var item pluginItem
		if err := json.Unmarshal(raw, &item); err != nil {
			c.log.Warn("skipping malformed plugin item", "source", id, "line", line, "error", err)
			continue
		}
		if item.Content == "" {
			// Content is the one required field; without it there is nothing
			// for the aggregator to read.
			c.log.Warn("skipping plugin item with no content", "source", id, "line", line, "url", item.URL)
			continue
		}

		collected := Collected{
			URL:     item.URL,
			Title:   item.Title,
			Content: item.Content,
		}
		if item.SourceTime != "" {
			// Provenance only. A time the engine cannot parse is dropped with a
			// warning rather than failing the item, and it never becomes the
			// day key either way.
			when, err := time.Parse(time.RFC3339, item.SourceTime)
			if err != nil {
				c.log.Warn("ignoring unparsable plugin source_time", "source", id, "line", line, "value", item.SourceTime)
			} else {
				collected.SourceTime = when
			}
		}
		items = append(items, collected)
	}

	return items
}

// readLine reads one newline-terminated line, bounded by limit. A line past the
// limit is discarded to its end and reported with tooLong, so reading continues
// with the next item instead of the whole stream stopping — which is what a
// bufio.Scanner would do, and would turn one oversize item into a lost batch.
func readLine(r *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return line, tooLong, err
		}
		if !tooLong {
			if len(line)+len(chunk) > limit {
				// Stop accumulating, but keep draining to the newline below.
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if !isPrefix {
			return line, tooLong, nil
		}
	}
}

// environment is the minimal environment of ADR-0003 §6: PATH, locale, and only
// the variables this source's config names. Nothing else leaks through — a
// plugin gets what it was granted, not whatever the engine was started with.
func environment(allowed []string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LANG=" + os.Getenv("LANG"),
		"LC_ALL=" + os.Getenv("LC_ALL"),
	}
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
