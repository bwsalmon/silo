//go:build !linux

package main

import (
	"errors"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

func openSiloNet(string) (siloNet, error) { return nil, errors.New("silo-net needs Linux") }

func sandboxDriver(sandboxConfig) (runtime.Driver, error) {
	return nil, errors.New("the sandbox class needs Linux")
}
