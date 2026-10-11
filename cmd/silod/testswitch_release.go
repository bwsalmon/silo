//go:build !silo_conformance

package main

import "context"

// A release build has no test switches (testswitch_conformance.go):
// both hooks are nil, and nothing watches for them.

const testSwitchesBuilt = false

func keepControlPortSwitch() func() bool { return nil }
func skipControlGateSwitch() func() bool { return nil }
func watchTestSwitches(context.Context)  {}
