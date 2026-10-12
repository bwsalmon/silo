package agents

import (
	"strings"
	"testing"
)

func TestReleasePrices(t *testing.T) {
	ps := ReleasePrices()
	if len(ps) == 0 {
		t.Fatal("no prices")
	}
	for _, p := range ps {
		if p.Provider == "" || p.Model == "" || p.Input <= 0 || p.Output <= 0 || p.CacheRead <= 0 || p.CacheWrite <= 0 {
			t.Errorf("price %+v", p)
		}
	}
	m := MergePrices([]Price{{Provider: "anthropic", Model: "claude-opus-5-5", Input: 1, Output: 2, CacheWrite: 3, CacheRead: 4}})
	for _, p := range m {
		if p.Model == "claude-opus-5-5" && (!p.Override || p.Input != 1) {
			t.Errorf("override not laid over: %+v", p)
		}
	}
	if len(m) != len(ps) {
		t.Errorf("merged %d, want %d", len(m), len(ps))
	}
	// Every adapter takes a provider the table prices.
	for _, a := range Adapters {
		ok := false
		for _, p := range ps {
			ok = ok || a.Takes(p.Provider)
		}
		if !ok {
			t.Errorf("adapter %s has no priced model", a.Name)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"fixer", "a", "t-1945"} {
		if err := ValidName(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"", "Fixer", "1x", "a_b", "x-", strings.Repeat("a", 60)} {
		if ValidName(n) == nil {
			t.Errorf("%q: want an error", n)
		}
	}
}

func TestStartupScript(t *testing.T) {
	s := StartupScript()
	for _, want := range []string{"#!/bin/sh", ToolsBinaryURL, ToolsVersion, "systemctl enable --now silo-tools.service", "arm64", "amd64"} {
		if !strings.Contains(s, want) {
			t.Errorf("startup script lacks %q", want)
		}
	}
}

func TestHoldsSession(t *testing.T) {
	for st, want := range map[string]bool{StateRunning: true, StateIdle: true, StateWaiting: true, StateProvisioning: true,
		StateStopped: false, StateBudgetExhausted: false, StateDeleting: false, StateFailed: false} {
		if HoldsSession(st) != want {
			t.Errorf("%s: %v", st, !want)
		}
	}
}
