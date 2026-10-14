package sandbox

import "github.com/bwsalmon/silo/v2/internal/lockfile"

// LockFile is the silo folder's lock file, relative to the folder.
const LockFile = lockfile.Path

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
	return lockfile.Set(folder, "images", images)
}

// ReadImages returns the images recorded in the folder's lock file, or
// nil when there is none.
func ReadImages(folder string) (map[string]Image, error) {
	var images map[string]Image
	_, err := lockfile.Get(folder, "images", &images)
	return images, err
}
