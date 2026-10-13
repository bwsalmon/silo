//go:build !linux

package gvisor

import "errors"

// RemapArg is the subcommand that runs RemapMain: `silod runtime-remap`.
const RemapArg = "runtime-remap"

// RemapMain needs Linux: the holder exists only there.
func RemapMain([]string) error { return errors.New("runtime-remap: the holder runs only on Linux") }
