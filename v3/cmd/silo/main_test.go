package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"help"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "gateway") {
		t.Fatalf("help: %v %q", err, out.String())
	}
	for _, want := range []string{"bridge", "connect", "mcp", "page", "secret", "call"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("usage missing %q: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "agent.message") {
		t.Fatalf("usage should not mention removed agent.message: %q", out.String())
	}
	if err := run(nil, &out, &errOut); err == nil {
		t.Fatal("no command accepted")
	}
	if err := run([]string{"frobnicate"}, &out, &errOut); err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestCall(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "gw.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan map[string]any, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var req map[string]any
		_ = json.Unmarshal(line, &req)
		got <- req
		_ = json.NewEncoder(c).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": []string{"root"}})
	}()
	var out bytes.Buffer
	if err := run([]string{"call", "-socket", sock, "agent.list", `{"state":["connected"]}`}, &out, &out); err != nil {
		t.Fatal(err)
	}
	req := <-got
	if req["method"] != "agent.list" || req["jsonrpc"] != "2.0" {
		t.Fatalf("request %v", req)
	}
	if !strings.Contains(out.String(), `"root"`) {
		t.Fatalf("output %q", out.String())
	}
	if err := run([]string{"call", "-socket", sock, "agent.list", "{bad"}, &out, &out); err == nil {
		t.Fatal("bad params accepted")
	}
}

func TestPageAndSecretCLI(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "br.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	var methods []string
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req map[string]any
			_ = json.Unmarshal(line, &req)
			m, _ := req["method"].(string)
			methods = append(methods, m)
			_ = json.NewEncoder(c).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req["id"],
				"result":  map[string]any{"ok": true, "method": m},
			})
			_ = c.Close()
		}
	}()

	var out, errOut bytes.Buffer
	for _, cmd := range [][]string{
		{"page", "add", "--socket", sock, "--slug", "app", "--title", "App", "--port", "3000"},
		{"page", "list", "--socket", sock},
		{"page", "rm", "--socket", sock, "app"},
		{"secret", "request", "--socket", sock, "--name", "api-key", "--reason", "testing"},
		{"secret", "list", "--socket", sock, "--pending"},
		{"secret", "provide", "--socket", sock, "--id", "sec-1", "--value", "topsecret"},
		{"secret", "reject", "--socket", sock, "--id", "sec-2", "--note", "nope"},
	} {
		if err := run(cmd, &out, &errOut); err != nil {
			t.Fatalf("run(%v): %v", cmd, err)
		}
	}

	wantMethods := []string{
		"page.register",
		"page.list",
		"page.unregister",
		"secret.request",
		"secret.list",
		"secret.provide",
		"secret.reject",
	}
	if strings.Join(methods, ",") != strings.Join(wantMethods, ",") {
		t.Fatalf("methods = %v, want %v", methods, wantMethods)
	}
}
