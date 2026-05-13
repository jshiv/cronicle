package configsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

// FileSource reads cronicle.hcl from a local path. Uses fsnotify to
// avoid re-reading + re-hashing on every refresh tick: the watcher
// flips a dirty bit on inode events, Fetch consults the bit and
// short-circuits when no change has happened since the last read.
//
// We watch the parent directory rather than the file path itself
// because most "edit" patterns rotate the inode (write to .tmp +
// atomic rename) and a path-level watch loses its target on rename.
// Filtering by basename inside the event handler keeps semantics
// clean.
//
// On platforms where fsnotify can't start (rare — Linux/macOS/Windows
// all have native backends), construction falls back to a polling
// model: dirty stays permanently true and Fetch always re-reads.
// The etag still short-circuits the parse step downstream.
type FileSource struct {
	Path    string
	watcher *fsnotify.Watcher
	dirty   atomic.Bool // set by watcher goroutine on relevant events

	// cachedEtag/cachedBytes hold the last successful read so we can
	// reuse them when dirty=false. Access only happens from the cron
	// goroutine (single-threaded today); the dirty flag is the only
	// cross-goroutine synchronization needed.
	cachedEtag  string
	cachedBytes []byte
}

// NewFileSource constructs a FileSource and starts the fsnotify
// watcher on the file's parent directory. The returned Source owns
// the watcher goroutine; call Close on shutdown to release it. If
// fsnotify can't initialize, returns a Source that polls on every
// Fetch (forces re-read each tick) — semantically correct, slightly
// more disk I/O.
func NewFileSource(path string) (*FileSource, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s := &FileSource{Path: abs}
	// dirty=true on construction so the first Fetch unconditionally
	// reads + populates the cache.
	s.dirty.Store(true)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		// Fall back to no-watcher mode: dirty stays true permanently.
		slog.Warn("file watcher unavailable; refreshes will re-read on every tick",
			"path", abs, "err", err.Error())
		return s, nil
	}
	if err := w.Add(filepath.Dir(abs)); err != nil {
		_ = w.Close()
		slog.Warn("file watcher add failed; refreshes will re-read on every tick",
			"dir", filepath.Dir(abs), "err", err.Error())
		return s, nil
	}
	s.watcher = w
	go s.watchLoop(w)
	return s, nil
}

// watchLoop drains the fsnotify event stream. Any event involving
// our path's basename (or a rename targeting it) flips the dirty
// bit; the next Fetch picks up the change.
//
// The watcher reference is captured locally so a concurrent Close
// (which nils s.watcher) can't race with the loop's channel reads.
// Close's path is: invoke watcher.Close() → fsnotify closes Events
// → the loop's receive returns ok=false → loop exits cleanly.
func (s *FileSource) watchLoop(w *fsnotify.Watcher) {
	base := filepath.Base(s.Path)
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			// Any event on our file (create/write/rename/remove/chmod)
			// is enough to invalidate. The Fetch path handles the
			// missing-file case with ErrNotFound.
			s.dirty.Store(true)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			// Watcher errors are non-fatal: surface and force a re-read
			// on the next tick so we don't get stuck with a stale cache
			// after a transient watch failure.
			slog.Warn("file watcher error", "path", s.Path, "err", err.Error())
			s.dirty.Store(true)
		}
	}
}

// Close shuts down the watcher goroutine. Safe to call multiple times.
// The Source remains usable after Close (falls back to polling).
func (s *FileSource) Close() error {
	if s.watcher == nil {
		return nil
	}
	err := s.watcher.Close()
	s.watcher = nil
	return err
}

func (s *FileSource) Fetch(_ context.Context, prevEtag string) ([]byte, string, bool, error) {
	// CompareAndSwap dirty=true → false so concurrent watcher events
	// don't get lost: if an event fires while we're reading, dirty
	// goes back to true and the NEXT Fetch will re-read.
	wasDirty := s.dirty.CompareAndSwap(true, false)
	if !wasDirty && prevEtag != "" && prevEtag == s.cachedEtag {
		// Cache hit: no fs activity, etag matches caller's view.
		return nil, prevEtag, false, nil
	}
	bytes, err := os.ReadFile(s.Path)
	if err != nil {
		// Restore dirty so the next tick re-attempts (file may reappear).
		s.dirty.Store(true)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, prevEtag, false, ErrNotFound
		}
		return nil, prevEtag, false, err
	}
	sum := sha256.Sum256(bytes)
	etag := hex.EncodeToString(sum[:])
	if etag == prevEtag {
		// Watcher fired but contents are identical (e.g. chmod-only).
		s.cachedEtag = etag
		s.cachedBytes = bytes
		return nil, etag, false, nil
	}
	s.cachedEtag = etag
	s.cachedBytes = bytes
	return bytes, etag, true, nil
}

func (s *FileSource) String() string {
	return "file://" + s.Path
}
