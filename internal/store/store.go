// Package store owns the on-disk layout defined by ADR-0002.
//
// Everything that writes into the data root goes through here, deliberately:
// ADR-0003 makes the engine the single writer so that the day key, the identity
// digest, the filename charset and the atomic-write rule have exactly one
// implementation. A collector — built-in or external — produces items and never
// touches the filesystem itself.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// DayFormat is the day-folder name: a UTC calendar date. ADR-0002 fixes the
// clock so collection, aggregation and retention share one boundary.
const DayFormat = "2006-01-02"

// keyLength is the number of hex characters of the identity digest kept in a
// filename — ADR-0002's "first 12 hex chars of SHA-256".
const keyLength = 12

// Item is one collected item as it is written to disk. The engine fills
// FetchedAt and Collector; a collector supplies the rest.
type Item struct {
	// Source is the config key of the source that produced this item. It
	// becomes a path component, so it carries the config's id constraints.
	Source string

	// URL is where the item came from, when it has an address. It is also the
	// identity the filename digest is taken over when present, which is what
	// makes a re-run rewrite the same file instead of appending a duplicate.
	URL string

	// Title is the source's own title, when it offers one.
	Title string

	// FetchedAt is stamped by the engine at ingestion and chooses the day
	// folder. ADR-0003 is explicit that a collector never chooses it, so a
	// plugin cannot write into a day that is supposed to be immutable.
	FetchedAt time.Time

	// SourceTime is a timestamp the item itself claimed (a feed's publication
	// date, a plugin's reported time). Provenance only — it never selects the
	// day folder.
	SourceTime time.Time

	// Collector is the collector type that produced the item: "feed", "http",
	// "inbox", or a plugin's configured name.
	Collector string

	// OriginalFilename records the name a file had in the inbox before it was
	// normalised, so a drained hand-off can still be traced back.
	OriginalFilename string

	// Content is the raw extracted text, unmodified. Layer 1 never summarises,
	// scores or trims it.
	Content string
}

// frontMatter is the provenance block written above an item's content. Field
// order here is the order on disk, and every optional field is omitted rather
// than written empty so a hand-read file carries no noise.
type frontMatter struct {
	Source           string `yaml:"source"`
	URL              string `yaml:"url,omitempty"`
	Title            string `yaml:"title,omitempty"`
	FetchedAt        string `yaml:"fetched_at"`
	SourceTime       string `yaml:"source_time,omitempty"`
	Collector        string `yaml:"collector"`
	OriginalFilename string `yaml:"original_filename,omitempty"`
}

// Store is a data root.
type Store struct {
	root string
}

// New returns a Store rooted at the configured data root.
func New(root string) *Store { return &Store{root: root} }

// Root is the data root itself.
func (s *Store) Root() string { return s.root }

// InboxDir is the watched folder external tools drop files into.
func (s *Store) InboxDir() string { return filepath.Join(s.root, "inbox") }

// DayDir is the folder for a given instant's UTC day.
func (s *Store) DayDir(t time.Time) string {
	return filepath.Join(s.root, "days", Day(t))
}

// ItemsDir is where a day's collected items live.
func (s *Store) ItemsDir(t time.Time) string {
	return filepath.Join(s.DayDir(t), "items")
}

// Day is the UTC day key for an instant.
func Day(t time.Time) string { return t.UTC().Format(DayFormat) }

// ItemKey is the stable identity digest ADR-0002 puts in a filename: taken over
// the URL when there is one, otherwise over the content. Keying on the URL is
// what makes a re-run of the same source on the same day rewrite the same file,
// even when the page's text has shifted since the first fetch.
func ItemKey(url, content string) string {
	identity := url
	if identity == "" {
		identity = content
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])[:keyLength]
}

// Filename is an item's name within its day: `<source-id>--<key>.md`.
func Filename(source, url, content string) string {
	return fmt.Sprintf("%s--%s.md", source, ItemKey(url, content))
}

// WriteItem writes one item into its UTC day folder and returns the path.
//
// The write is atomic — a temporary file in the destination directory followed
// by a rename — so a re-run never exposes a torn file to an aggregator reading
// the same directory.
func (s *Store) WriteItem(item Item) (string, error) {
	if item.Source == "" {
		return "", errors.New("item has no source")
	}
	if item.Collector == "" {
		return "", errors.New("item has no collector")
	}
	if item.FetchedAt.IsZero() {
		return "", errors.New("item has no fetched_at: the engine must stamp it")
	}

	dir := s.ItemsDir(item.FetchedAt)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create items directory: %w", err)
	}

	body, err := render(item)
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, Filename(item.Source, item.URL, item.Content))
	if err := writeFileAtomic(path, body); err != nil {
		return "", err
	}
	return path, nil
}

// render builds the front-matter-plus-content bytes for an item.
func render(item Item) ([]byte, error) {
	fm := frontMatter{
		Source:           item.Source,
		URL:              item.URL,
		Title:            item.Title,
		FetchedAt:        item.FetchedAt.UTC().Format(time.RFC3339),
		Collector:        item.Collector,
		OriginalFilename: item.OriginalFilename,
	}
	if !item.SourceTime.IsZero() {
		fm.SourceTime = item.SourceTime.UTC().Format(time.RFC3339)
	}

	encoded, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("encode front matter: %w", err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(encoded)
	b.WriteString("---\n")
	b.WriteString(item.Content)
	// A trailing newline keeps the file well-formed for line-oriented tools
	// when the content does not end with one.
	if !strings.HasSuffix(item.Content, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory and a rename. Same-directory matters: rename is only atomic within
// a filesystem, and a temp dir elsewhere may be on a different one.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()

	// From here on any failure must not leave the temporary file behind.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary file: %w", err)
	}
	// Sync before the rename: a rename that lands before the data reaches disk
	// would survive a crash as a correctly-named empty file, which is worse
	// than no file at all.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temporary file: %w", err)
	}
	// CreateTemp makes the file 0600; items are meant to be readable by the
	// operator's own tooling, so widen it before it becomes visible under its
	// real name.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("set permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// SourceLastCollected reports the most recent UTC day on which the source wrote
// any item, searching back at most maxDays from now. The second result is false
// when it never did within that window.
//
// Deriving this from the layout rather than from a state file is deliberate:
// the files already answer the question, and ADR-0002 assigns state.json to the
// aggregator. A second bookkeeping file for the collector would be a second
// thing to keep true.
func (s *Store) SourceLastCollected(source string, now time.Time, maxDays int) (time.Time, bool, error) {
	prefix := source + "--"

	for back := 0; back <= maxDays; back++ {
		day := now.UTC().AddDate(0, 0, -back)

		entries, err := os.ReadDir(s.ItemsDir(day))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return time.Time{}, false, fmt.Errorf("read items directory: %w", err)
		}

		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			// The prefix match is on `<source>--`, so a source id cannot match
			// another whose id it is a prefix of: `hn` never matches
			// `hn-front--….md` because the separator has to follow immediately.
			if strings.HasPrefix(e.Name(), prefix) {
				return day, true, nil
			}
		}
	}

	return time.Time{}, false, nil
}
