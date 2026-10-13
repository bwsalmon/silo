//go:build !linux

package main

import "errors"

func openSiloNet(string) (siloNet, error) { return nil, errors.New("silo-net needs Linux") }
