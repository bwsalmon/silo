//go:build !linux

package nsd

import "errors"

func getxattr(path, name string) bool { return false }

func tryUserNS() error { return errors.New("user namespaces need Linux") }
