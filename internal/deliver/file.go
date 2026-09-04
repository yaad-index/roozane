package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yaad-index/roozane/internal/config"
)

// fileParams is what a `type: file` sink configures.
type fileParams struct {
	// Path is where the digest is written. A `{day}` placeholder is replaced
	// with the digest's UTC day, so a config can keep every day or overwrite
	// one file — both are reasonable and the choice is the reader's.
	Path string `yaml:"path"`

	// Format selects which artifact is written: "markdown" (default) or "json".
	Format string `yaml:"format"`
}

// dayPlaceholder is what a path may contain to get one file per day.
const dayPlaceholder = "{day}"

// fileSink copies the digest to a path.
type fileSink struct {
	path   string
	format string
}

func newFileSink(sink config.Sink) (Sink, error) {
	var params fileParams
	if err := sink.DecodeParams(&params); err != nil {
		return nil, fmt.Errorf("file sink params: %w", err)
	}
	if params.Path == "" {
		return nil, errors.New("file sink needs params.path")
	}

	format := params.Format
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "json" {
		return nil, fmt.Errorf("file sink format %q must be markdown or json", format)
	}

	return &fileSink{path: params.Path, format: format}, nil
}

// Deliver writes the digest. The write is atomic for the same reason every
// other write in the engine is: whatever reads this file — a static site build,
// a sync tool — must never see half a digest.
func (s *fileSink) Deliver(_ context.Context, digest Digest) error {
	path := strings.ReplaceAll(s.path, dayPlaceholder, digest.Day)

	var data []byte
	switch s.format {
	case "json":
		// Re-indent nothing: the bytes on disk are the contract sinks were
		// promised, so they are copied through unchanged.
		if !json.Valid(digest.Structured) {
			return errors.New("structured digest is not valid JSON")
		}
		data = digest.Structured
	default:
		data = []byte(digest.Markdown)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := writeAtomic(path, data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeAtomic is a temp-then-rename write into the destination's own directory.
//
// It is a small duplicate of the store's writer on purpose: the store owns the
// engine's data root and nothing else should write there, while this writes to
// an arbitrary operator-chosen path. Sharing one function would invite calling
// the data-root writer with a foreign path.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	name := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
