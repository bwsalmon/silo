//go:build silo_conformance

package main

// The conformance build's test-only switches (docs/silo/testing.md,
// "Sealed and read silos"). This file is compiled only with the
// silo_conformance tag, so a release image has neither switch, nor
// their names; testswitch_test.go checks that.
//
//   - SILO_TEST_KEEP_CONTROL_PORT keeps a sealed silo's api-out fence
//     rule, so a probe in it reaches the gateway's API port and the
//     gateway's and silod's own refusals are seen without the fence's.
//   - SILO_TEST_SKIP_CONTROL_GATE turns off the gateway's control check,
//     so silod's check is seen alone.
//
// Each is on when its environment variable is set in the container, or
// when its name is a line of the switches file. silod keeps that file
// in step with the control VM's metadata attribute silo-test-switches,
// which the conformance suite sets with the admin's key: the suite can
// flip a switch on a running install, and the gateway, which has no
// route to the metadata server, reads the file silod writes into the
// /run/silo it shares.

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	switchKeepControlPort = "SILO_TEST_KEEP_CONTROL_PORT"
	switchSkipControlGate = "SILO_TEST_SKIP_CONTROL_GATE"

	switchesAttribute = "silo-test-switches"
)

// switchesFile is where silod writes the switches and the gateway reads
// them: /run/silo, which both containers mount.
func switchesFile() string {
	if f := os.Getenv("SILO_TEST_SWITCHES_FILE"); f != "" {
		return f
	}
	return "/run/silo/test-switches"
}

// testSwitchesBuilt reports whether this build has the switches.
const testSwitchesBuilt = true

var switchCache struct {
	sync.Mutex
	read time.Time
	on   map[string]bool
}

// testSwitch is the switch name's current state, read again at most
// once a second.
func testSwitch(name string) func() bool {
	return func() bool {
		if os.Getenv(name) != "" {
			return true
		}
		switchCache.Lock()
		defer switchCache.Unlock()
		if time.Since(switchCache.read) > time.Second {
			switchCache.read = time.Now()
			switchCache.on = map[string]bool{}
			if b, err := os.ReadFile(switchesFile()); err == nil {
				for _, l := range strings.Fields(strings.ReplaceAll(string(b), ",", " ")) {
					switchCache.on[l] = true
				}
			}
		}
		return switchCache.on[name]
	}
}

func keepControlPortSwitch() func() bool { return testSwitch(switchKeepControlPort) }
func skipControlGateSwitch() func() bool { return testSwitch(switchSkipControlGate) }

// watchTestSwitches keeps the switches file in step with the control
// VM's silo-test-switches metadata attribute, until ctx ends.
func watchTestSwitches(ctx context.Context) {
	log.Printf("conformance build: test switches %s and %s are compiled in", switchKeepControlPort, switchSkipControlGate)
	url := "http://metadata.google.internal/computeMetadata/v1/instance/attributes/" + switchesAttribute
	cur := "\x00"
	for ctx.Err() == nil {
		val := ""
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Metadata-Flavor", "Google")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				val = strings.TrimSpace(string(b))
			}
		}
		if val != cur {
			f := switchesFile()
			os.MkdirAll(filepath.Dir(f), 0o750)
			if err := os.WriteFile(f+".tmp", []byte(val+"\n"), 0o640); err == nil && os.Rename(f+".tmp", f) == nil {
				log.Printf("conformance build: test switches now %q", val)
				cur = val
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}
