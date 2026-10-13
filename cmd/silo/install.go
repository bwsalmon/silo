package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/units"
	"github.com/bwsalmon/silo/pkg/install"
	installgcp "github.com/bwsalmon/silo/pkg/install/gcp"
	localinstall "github.com/bwsalmon/silo/pkg/install/local"
)

// version is this CLI's release, the default of silo install --version.
// Releases set it with -ldflags "-X main.version=...".
var version = "dev"

// newCloud connects install's GCP APIs with the installer's own
// credentials. Tests replace it.
var newCloud = installgcp.New

// errNotApplied ends a run whose plan failed or that wasn't confirmed:
// what went wrong has been printed.
var errNotApplied = errors.New("nothing was changed")

// installFlags parses an install or uninstall command line, which has
// no positional arguments.
func (c *cli) installFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(c.stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// install is silo install (docs/silo/install.md): it plans every step,
// prints the plan, asks for confirmation unless --yes, and applies it.
func (c *cli) install(ctx context.Context, args []string) error {
	if backend, rest := backendFlag(args); backend == "local" {
		return c.installLocal(ctx, rest)
	} else if backend != "" && backend != "gcp" {
		return usagef("--backend %q: want gcp or local", backend)
	} else {
		args = rest
	}
	cfg := install.Defaults()
	fs := flag.NewFlagSet("silo install", flag.ContinueOnError)
	cfg.Flags(fs, version)
	if err := c.installFlags(fs, args); err != nil {
		return err
	}
	c.output = cfg.Output
	if err := cfg.Validate(); err != nil {
		return usageError{strings.ReplaceAll(err.Error(), "\n", "\n  ")}
	}
	if cfg.Output == "json" && !cfg.DryRun {
		return usagef("--output json is for --dry-run: an install prints its plan as a table")
	}
	cloud, err := newCloud(ctx, cfg.Credentials, version)
	if err != nil {
		return err
	}
	// Steps 13 to 15 reach silod through the front door, as the
	// installer: the tunnel opens on the first call, which a step makes
	// only once the control VM exists.
	fd, err := c.newFrontDoor(&frontDoorFlags{project: cfg.Project, instance: install.ControlVM})
	if err != nil {
		return err
	}
	defer fd.Close()
	if cloud.Silod == nil {
		if cloud.Silod, err = install.NewSilod(fd); err != nil {
			return err
		}
	}
	env := install.NewEnv(cfg, cloud)
	env.Progress = c.stdout
	steps := install.Steps(env)
	plans := install.PlanAll(ctx, steps)
	header := fmt.Sprintf("silo install: project %s, home region %s", cfg.Project, cfg.HomeRegion())
	return c.plan(header, plans, cfg.DryRun, cfg.Yes, "Apply this plan?", func() error {
		return install.Apply(ctx, steps, c.stdout)
	})
}

// backendFlag takes --backend out of args: install's backends have
// flag sets of their own.
func backendFlag(args []string) (backend string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return backend, append(rest, args[i:]...)
		case a == "--backend" || a == "-backend":
			if i+1 < len(args) {
				backend = args[i+1]
				i++
				continue
			}
		case strings.HasPrefix(a, "--backend=") || strings.HasPrefix(a, "-backend="):
			_, backend, _ = strings.Cut(a, "=")
			continue
		}
		rest = append(rest, a)
	}
	return backend, rest
}

// newLocalHost is the machine silo install --backend local changes.
// Tests replace it.
var newLocalHost = localinstall.LocalHost

// installLocal is silo install --backend local: the local backend's
// units, rootless as the current user, or with --system under a silo
// account (docs/silo/local-rootless-storage.md, calls L33 and L34).
func (c *cli) installLocal(ctx context.Context, args []string) error {
	var cfg localinstall.Config
	var system, dryRun, yes bool
	var output string
	fs := flag.NewFlagSet("silo install --backend local", flag.ContinueOnError)
	fs.BoolVar(&system, "system", false, "the system install: run as root, create the silo account, and write system units with User=silo")
	fs.StringVar(&cfg.DataDir, "data-dir", "", "the data directory (default ~/.local/share/silo, or /var/lib/silo with --system)")
	fs.StringVar(&cfg.Silod, "silod", "", "the silod binary the units run (default the one beside silo, else on PATH)")
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan and change nothing")
	fs.BoolVar(&yes, "yes", false, "don't ask for confirmation")
	fs.StringVar(&output, "output", "table", "the plan's format: table or json")
	fs.StringVar(&output, "o", "table", "short for --output")
	if err := c.installFlags(fs, args); err != nil {
		return err
	}
	c.output = output
	if output != "table" && output != "json" {
		return usagef("--output %q: want table or json", output)
	}
	if output == "json" && !dryRun {
		return usagef("--output json is for --dry-run: an install prints its plan as a table")
	}
	cfg.Mode = units.Rootless
	if system {
		cfg.Mode = units.System
	}
	cfg, err := localinstall.Defaults(cfg)
	if err != nil {
		return err
	}
	h := newLocalHost()
	h.Progress = c.stdout
	steps := localinstall.Steps(cfg, h)
	plans := install.PlanAll(ctx, steps)
	header := fmt.Sprintf("silo install: local backend, %s, silod as %s, data directory %s", cfg.Mode, cfg.User, cfg.DataDir)
	return c.plan(header, plans, dryRun, yes, "Apply this plan?", func() error {
		return install.Apply(ctx, steps, c.stdout)
	})
}

// uninstall is silo uninstall: install's steps undone in reverse order.
func (c *cli) uninstall(ctx context.Context, args []string) error {
	cfg := install.Defaults()
	fs := flag.NewFlagSet("silo uninstall", flag.ContinueOnError)
	fs.StringVar(&cfg.Project, "project", "", "the host project (required)")
	fs.StringVar(&cfg.Credentials, "credentials", "", "a service-account key file (default Application Default Credentials)")
	fs.BoolVar(&cfg.Force, "force", false, "also delete every silo, agent and object of the tree, and orphaned silo objects")
	fs.BoolVar(&cfg.DeleteData, "delete-data", false, "also delete the Cloud SQL instance, keeping a final backup for 30 days")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "print the plan and change nothing")
	fs.BoolVar(&cfg.Yes, "yes", false, "don't ask for confirmation")
	fs.StringVar(&cfg.Output, "output", "table", "the plan's format: table or json")
	fs.StringVar(&cfg.Output, "o", "table", "short for --output")
	if err := c.installFlags(fs, args); err != nil {
		return err
	}
	c.output = cfg.Output
	if cfg.Project == "" {
		return usagef("--project is required")
	}
	if cfg.Output != "table" && cfg.Output != "json" {
		return usagef("--output %q: want table or json", cfg.Output)
	}
	if cfg.Output == "json" && !cfg.DryRun {
		return usagef("--output json is for --dry-run")
	}
	cloud, err := newCloud(ctx, cfg.Credentials, version)
	if err != nil {
		return err
	}
	if err := install.LoadRecord(ctx, &cfg, cloud); err != nil {
		return fmt.Errorf("reading the install's record: %w", err)
	}
	// The tenants, orphans and credentials steps ask silod, through the
	// front door, what the tree holds; the tunnel opens on the first
	// call, which a step makes only if silo-control exists.
	fd, err := c.newFrontDoor(&frontDoorFlags{project: cfg.Project, instance: install.ControlVM})
	if err != nil {
		return err
	}
	defer fd.Close()
	if cloud.Silod == nil {
		if cloud.Silod, err = install.NewSilod(fd); err != nil {
			return err
		}
	}
	steps := install.UninstallSteps(install.NewEnv(cfg, cloud))
	plans := install.PlanRemoveAll(ctx, steps)
	return c.plan("silo uninstall: project "+cfg.Project, plans, cfg.DryRun, cfg.Yes, uninstallPrompt(plans),
		func() error { return install.Remove(ctx, steps, c.stdout) })
}

// uninstallPrompt is uninstall's confirmation, which names the database
// instance when the plan deletes it.
func uninstallPrompt(plans []install.StepPlan) string {
	for _, p := range plans {
		if p.Step != "db-instance" {
			continue
		}
		for _, ch := range p.Changes {
			if ch.Action == install.ActionDelete {
				return fmt.Sprintf("Delete everything above marked delete, including the Cloud SQL instance %s and every grant, policy and audit record in it (a final backup is kept %d days)?",
					ch.Object, install.FinalBackupDays)
			}
		}
	}
	return "Delete everything above marked delete?"
}

// plan prints plans, and unless dryRun, asks prompt (unless yes) and
// runs apply.
func (c *cli) plan(header string, plans []install.StepPlan, dryRun, yes bool, prompt string, apply func() error) error {
	var err error
	if c.output == "json" {
		err = install.RenderJSON(c.stdout, plans)
	} else {
		err = install.RenderTable(c.stdout, header, plans)
	}
	if err != nil {
		return err
	}
	failed, changing := false, false
	for _, p := range plans {
		failed = failed || p.Failed()
		changing = changing || p.Changing()
	}
	switch {
	case failed:
		return errNotApplied
	case dryRun, !changing:
		return nil
	case !yes && !c.confirm(prompt):
		return errNotApplied
	}
	return apply()
}

// confirm asks prompt on stderr and reads y or yes from stdin.
func (c *cli) confirm(prompt string) bool {
	fmt.Fprintf(c.stderr, "\n%s [y/N] ", prompt)
	if c.stdin == nil {
		return false
	}
	line, err := bufio.NewReader(c.stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
