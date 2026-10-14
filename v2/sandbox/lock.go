package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LockFile is the silo folder's lock file, relative to the folder.
const LockFile = ".silo/lock.json"

// Image is what a sandbox's image resolved to.
type Image struct {
	// Ref is what the Silofile named: an image reference, or the
	// Dockerfile for build:.
	Ref string `json:"ref"`
	// Digest is the content digest it resolved to: a repository digest
	// (name@sha256:...) for a pulled image, the image id (sha256:...)
	// for a built one.
	Digest string `json:"digest"`
}

// RecordImages writes images, by sandbox name, into the folder's
// .silo/lock.json under "images", replacing that key and keeping every
// other key as it was (stage 3a adds the grants summary beside it). The
// file is replaced atomically.
func RecordImages(folder string, images map[string]Image) error {
	path := filepath.Join(folder, LockFile)
	doc := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := json.Marshal(images)
	if err != nil {
		return err
	}
	doc["images"] = raw
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

// ReadImages returns the images recorded in the folder's lock file, or
// nil when there is none.
func ReadImages(folder string) (map[string]Image, error) {
	data, err := os.ReadFile(filepath.Join(folder, LockFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Images map[string]Image `json:"images"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.Images, nil
}
