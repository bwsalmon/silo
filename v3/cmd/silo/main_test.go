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
		json.Unmarshal(line, &req)
		got <- req
		json.NewEncoder(c).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": []string{"root"}})
	}()
	var out bytes.Buffer
	if err := run([]string{"call", "-socket", sock, "list", `{"state":["running"]}`}, &out, &out); err != nil {
		t.Fatal(err)
	}
	req := <-got
	if req["method"] != "list" || req["jsonrpc"] != "2.0" {
		t.Fatalf("request %v", req)
	}
	if !strings.Contains(out.String(), `"root"`) {
		t.Fatalf("output %q", out.String())
	}
	if err := run([]string{"call", "-socket", sock, "list", "{bad"}, &out, &out); err == nil {
		t.Fatal("bad params accepted")
	}
}
