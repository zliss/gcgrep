// Package walkdir is a directory walker shared by the indexer and the
// watcher. Unlike filepath.WalkDir it can optionally follow symlinked
// directories (rg's -L), guarding against cycles by tracking resolved
// real paths.
package walkdir

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Fn receives the physical path, the dir flag and the FileInfo of the
// entry (already resolved for symlinks when follow is on). Returning
// fs.SkipDir on a directory skips its subtree.
type Fn func(path string, isDir bool, fi os.FileInfo) error

// Walk traverses root. With follow=false symlinks are reported neither
// as files nor dirs (rg default). With follow=true, symlinks to files
// are indexed and symlinks to directories are descended into once
// (cycles detected via EvalSymlinks).
func Walk(root string, follow bool, fn Fn) error {
	seen := map[string]bool{}
	if follow {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			seen[real] = true
		}
	}
	return walk(root, follow, seen, fn)
}

func walk(dir string, follow bool, seen map[string]bool, fn Fn) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // unreadable dir: skip subtree, caller counts via stat failures
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		isLink := e.Type()&fs.ModeSymlink != 0
		var fi os.FileInfo
		var ferr error
		if isLink {
			if !follow {
				continue
			}
			fi, ferr = os.Stat(path) // resolves the link
		} else {
			fi, ferr = e.Info()
		}
		if ferr != nil {
			continue
		}
		if fi.IsDir() {
			if isLink {
				real, rerr := filepath.EvalSymlinks(path)
				if rerr != nil || seen[real] {
					continue // cycle or dangling
				}
				seen[real] = true
			}
			if err := fn(path, true, fi); err == fs.SkipDir {
				continue
			}
			if err := walk(path, follow, seen, fn); err != nil {
				return err
			}
			continue
		}
		if fi.Mode().IsRegular() {
			if err := fn(path, false, fi); err != nil && err != fs.SkipDir {
				return err
			}
		}
	}
	return nil
}
