// Package watch wraps fsnotify with recursive directory registration and
// event debouncing. Events are batched and delivered after a quiet period
// so editors and git operations producing event bursts trigger one update.
//
// Linux note: fsnotify/inotify requires a watch per directory, which this
// recursive walk provides; the per-user inotify limit applies but is not
// handled specially in v1.
package watch

import (
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debounce = 200 * time.Millisecond

// Batch is one debounced set of changes. Rescan signals event loss
// (e.g. ReadDirectoryChangesW buffer overflow): the consumer must do a
// full reconcile instead of trusting Paths.
type Batch struct {
	Paths  []string // absolute paths whose state must be re-checked
	Rescan bool
}

type Watcher struct {
	fw     *fsnotify.Watcher
	root   string
	ignore func(rel string, isDir bool) bool
	out    chan Batch
	done   chan struct{}
}

// New starts watching root recursively. ignore filters directories from
// registration (rel is slash-separated, relative to root). Batches are
// delivered on C(); the channel closes on Close.
func New(root string, ignore func(rel string, isDir bool) bool) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fw: fw, root: root, ignore: ignore, out: make(chan Batch, 16), done: make(chan struct{})}
	if err := w.addRecursive(root); err != nil {
		fw.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

func (w *Watcher) C() <-chan Batch { return w.out }

func (w *Watcher) Close() {
	close(w.done)
	w.fw.Close()
}

// AddRecursive registers dir and all non-ignored subdirectories.
// Exported so the consumer can register directories created after start.
func (w *Watcher) AddRecursive(dir string) error { return w.addRecursive(dir) }

func (w *Watcher) addRecursive(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // racing deletes are normal; the next batch corrects
		}
		if !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(w.root, path)
		if rerr == nil && rel != "." && w.ignore(filepath.ToSlash(rel), true) {
			return filepath.SkipDir
		}
		if aerr := w.fw.Add(path); aerr != nil {
			// directory may have vanished between walk and Add
			return nil
		}
		return nil
	})
}

func (w *Watcher) loop() {
	defer close(w.out)
	pending := make(map[string]struct{})
	rescan := false
	var timer *time.Timer
	var fire <-chan time.Time
	reset := func() {
		if timer == nil {
			timer = time.NewTimer(debounce)
			fire = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(debounce)
	}
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fw.Events:
			if !ok {
				return
			}
			pending[ev.Name] = struct{}{}
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					rel, rerr := filepath.Rel(w.root, ev.Name)
					if rerr != nil || !w.ignore(filepath.ToSlash(rel), true) {
						// register before debounce fires so nested events
						// inside the new tree are not missed
						_ = w.addRecursive(ev.Name)
					}
				}
			}
			reset()
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			if err == fsnotify.ErrEventOverflow {
				rescan = true
				reset()
			}
			// other errors are transient (e.g. watched dir removed);
			// the corresponding Remove event handles cleanup
		case <-fire:
			batch := Batch{Rescan: rescan, Paths: make([]string, 0, len(pending))}
			for p := range pending {
				batch.Paths = append(batch.Paths, p)
			}
			pending = make(map[string]struct{})
			rescan = false
			fire = nil
			timer = nil
			select {
			case w.out <- batch:
			case <-w.done:
				return
			}
		}
	}
}
