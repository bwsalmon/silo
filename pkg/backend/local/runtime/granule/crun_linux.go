//go:build linux

package granule

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
)

// CrunArg is the subcommand that runs CrunMain: `silod runtime-crun`.
const CrunArg = "runtime-crun"

// CrunMain is `silod runtime-crun --crun <path> -- <crun args>`, the
// runc the vm class's shim calls, inside the holder: it gives the
// container's bundle to the container's root, then replaces itself with
// crun. The shim makes the bundle 0700 to the holder's root, and only
// opens it to a container's root when the spec maps ids; a VM
// container joins its user namespace by path instead, with no
// mappings, and crun, once inside that namespace, can't open the
// bundle's rootfs. The root is the spec's rootless.AnnotationRoot,
// which the driver sets.
func CrunMain(args []string) error {
	fs := flag.NewFlagSet(CrunArg, flag.ContinueOnError)
	crun := fs.String("crun", "crun", "the crun binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rargs := fs.Args()
	if crunCommand(rargs) == "create" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := prepareCrunBundle(rootless.RunscBundle(rargs, cwd)); err != nil {
			return fmt.Errorf("runtime-crun: bundle: %w", err)
		}
	}
	return syscall.Exec(*crun, append([]string{*crun}, rargs...), os.Environ())
}

// crunCommand is a runc command line's command: its first argument
// that isn't a global flag or a global flag's value.
func crunCommand(args []string) string {
	valued := map[string]bool{"--root": true, "--log": true, "--log-format": true, "--cgroup-manager": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case valued[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

// prepareCrunBundle chowns bundle to the container's root, if its spec
// names one.
func prepareCrunBundle(bundle string) error {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return err
	}
	var spec struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return err
	}
	v, ok := spec.Annotations[rootless.AnnotationRoot]
	if !ok {
		return nil
	}
	root, err := strconv.Atoi(v)
	if err != nil || root <= 0 {
		return errors.New(rootless.AnnotationRoot + " is " + strconv.Quote(v))
	}
	return os.Chown(bundle, root, root)
}

// CrunWrapperScript is the runc shim's BinaryName for the vm class: a
// script that runs `silod runtime-crun` with crun, since the shim's
// options take one path. env is set for it.
func CrunWrapperScript(silod, crun string, env []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("exec")
	if len(env) > 0 {
		b.WriteString(" env")
		for _, e := range env {
			b.WriteString(" " + shellQuote(e))
		}
	}
	fmt.Fprintf(&b, " %s %s --crun %s -- \"$@\"\n", shellQuote(silod), CrunArg, shellQuote(crun))
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
