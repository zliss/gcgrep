package shard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manifest records the set of valid shard files and the next sequence number.
// It is persisted as manifest.json in the shard directory.
type Manifest struct {
	Version int             `json:"version"`
	SeqNo   int             `json:"seqNo"`
	Shards  []ManifestShard `json:"shards"`
}

// ManifestShard is one shard entry in the manifest.
type ManifestShard struct {
	File    string `json:"file"`
	PathMin string `json:"pathMin"`
	PathMax string `json:"pathMax"`
}

const manifestFile = "manifest.json"

// WriteManifest atomically writes a manifest to dir/manifest.json.
func WriteManifest(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, manifestFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err == nil {
		_ = f.Sync()
		f.Close()
	}
	return os.Rename(tmp, filepath.Join(dir, manifestFile))
}

// ReadManifest reads manifest.json from dir. Returns nil if not found.
func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// CleanOrphans removes .idx and .idx.tmp files in dir that are not
// listed in the manifest.
func CleanOrphans(dir string, m *Manifest) {
	valid := make(map[string]bool)
	if m != nil {
		for _, s := range m.Shards {
			valid[s.File] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == manifestFile || name == manifestFile+".tmp" {
			continue
		}
		if !strings.HasSuffix(name, ".idx") && !strings.HasSuffix(name, ".idx.tmp") {
			continue
		}
		if !valid[name] {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// ManifestFromShards builds a Manifest from a slice of open shards.
func ManifestFromShards(shards []*Shard, seqNo int) *Manifest {
	m := &Manifest{Version: 1, SeqNo: seqNo}
	for _, s := range shards {
		m.Shards = append(m.Shards, ManifestShard{
			File:    filepath.Base(s.path),
			PathMin: s.pathMin,
			PathMax: s.pathMax,
		})
	}
	return m
}
