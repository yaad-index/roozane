package collect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yaad-index/roozane/internal/store"
)

// inboxSource is the source id inbox-drained items are filed under. It is not a
// configurable source — ADR-0002 §3 makes the inbox a built-in drain — but
// every item still needs a source component in its filename, and a fixed id
// keeps drained items grouped and greppable.
const inboxSource = "inbox"

// drainInbox moves everything waiting in the inbox into the current day's
// items, deleting each original only after its normalised item is atomically in
// place. The inbox is a queue, not an archive.
//
// A file that fails to convert is logged and left where it is: dropping it
// would lose a hand-off the reader deliberately placed there, and the next run
// gets another attempt.
func (r *Runner) drainInbox(now time.Time) (int, error) {
	dir := r.store.InboxDir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No inbox yet is the normal state of a fresh install, not a fault.
			return 0, nil
		}
		return 0, fmt.Errorf("read inbox: %w", err)
	}

	// Stable order so a run's log reads the same way twice.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !drainable(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var drained int
	var problems []error

	for _, name := range names {
		path := filepath.Join(dir, name)

		content, err := os.ReadFile(path) //nolint:gosec // paths come from the engine's own data root
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s: %w", name, err))
			r.log.Error("inbox file unreadable, leaving it in place", "file", name, "error", err)
			continue
		}
		if len(content) == 0 {
			// An empty drop carries nothing for the aggregator to read. Left in
			// place rather than deleted, since deleting a file the reader put
			// there deliberately is not this code's call to make.
			r.log.Warn("inbox file is empty, leaving it in place", "file", name)
			continue
		}

		text, truncated := capContent(string(content))
		if truncated {
			r.log.Warn("inbox file truncated at the size cap", "file", name, "cap_bytes", maxItemBytes)
		}

		if _, err := r.store.WriteItem(store.Item{
			Source:           inboxSource,
			Title:            name,
			FetchedAt:        now,
			Collector:        inboxSource,
			OriginalFilename: name,
			Content:          text,
		}); err != nil {
			problems = append(problems, fmt.Errorf("write item for %s: %w", name, err))
			r.log.Error("could not file inbox drop, leaving it in place", "file", name, "error", err)
			continue
		}

		// Only now: the normalised item is on disk under its real name, so
		// removing the original cannot lose the hand-off.
		if err := os.Remove(path); err != nil {
			// The item was written, so the drop is not lost — but the next run
			// would file it again. Worth reporting loudly.
			problems = append(problems, fmt.Errorf("remove drained %s: %w", name, err))
			r.log.Error("drained file could not be removed; it will be filed again next run", "file", name, "error", err)
			continue
		}

		drained++
	}

	return drained, errors.Join(problems...)
}

// drainable reports whether an inbox entry is a finished drop.
//
// ADR-0002 §3 requires producers to write atomically — build under a temporary
// name, then rename into the inbox — and pairs that with this skip rule. Both
// halves are needed: without the rename a half-written file could be read, and
// without this skip the in-progress temporary name would be read instead. That
// is what makes a partial drop unreadable by contract rather than by luck.
func drainable(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.HasSuffix(name, ".tmp")
}
