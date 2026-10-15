// Package lockfile is a silo folder's .silo/lock.json: what its last
// run resolved, one top-level key per part of silo (images, mcp, and
// stage 3a's grants), each written without disturbing the others.
package lockfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Path is the lock file, relative to the silo folder.
const Path = ".silo/lock.json"

// mu serialises writers within the process: a session's tool providers
// run in parallel, and each sets its own key.
var mu sync.Mutex

// Set writes v under key, keeping every other key as it was. The file
// is replaced atomically.
func Set(folder, key string, v any) error {
	mu.Lock()
	defer mu.Unlock()
	path := filepath.Join(folder, Path)
	doc := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	doc[key] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lock-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Get reads key into v. It reports false, with no error, when there is
// no lock file or no such key.
func Get(folder, key string, v any) (bool, error) {
	data, err := os.ReadFile(filepath.Join(folder, Path))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, err
	}
	raw, ok := doc[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}
