//go:build !linux

package granule

import "errors"

// WrapArg is the subcommand that runs WrapMain: `silod runtime-wrap`.
const WrapArg = "runtime-wrap"

// WrapMain needs Linux's mounts.
func WrapMain([]string) error { return errors.New("runtime-wrap: needs linux") }
