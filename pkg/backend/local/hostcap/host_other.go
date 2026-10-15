//go:build !linux

package hostcap

import "errors"

var errLinux = errors.New("the local backend needs Linux")

func memTotal() (int64, error)     { return 0, errLinux }
func diskGB(string) (int64, error) { return 0, errLinux }
