//go:build !linux || !cgo

package nsexec

// Available reports whether this binary carries the constructor. It
// needs Linux and cgo; without them the holder and silo unshare refuse.
const Available = false
