package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bwsalmon/silo/old/internal/procgroup"
	"github.com/bwsalmon/silo/old/location"
)

// Kubectl runs the system kubectl against one context and namespace.
// silo drives Kubernetes through kubectl rather than a Go client, as it
// drives ssh through OpenSSH (design §5.3, call 7): the user's
// kubeconfig, exec credential plugins (gke-gcloud-auth-plugin) and
// proxies behave exactly as at their own prompt, and silo takes on no
// client library.
type Kubectl struct {
	// Binary is kubectl, found on PATH when empty.
	Binary string
	// Context is the kubeconfig context; kubectl's current one when
	// empty.
	Context string
	// Namespace is the namespace; the context's when empty.
	Namespace string
	// Kubeconfig, when set, is passed as --kubeconfig.
	Kubeconfig string
}

// Argv is the full command line for kubectl args.
func (k *Kubectl) Argv(args ...string) []string {
	bin := k.Binary
	if bin == "" {
		bin = "kubectl"
	}
	argv := []string{bin}
	if k.Kubeconfig != "" {
		argv = append(argv, "--kubeconfig", k.Kubeconfig)
	}
	if k.Context != "" {
		argv = append(argv, "--context", k.Context)
	}
	if k.Namespace != "" {
		argv = append(argv, "--namespace", k.Namespace)
	}
	return append(argv, args...)
}

// Where names the context and namespace for messages.
func (k *Kubectl) Where() string {
	c, n := k.Context, k.Namespace
	if c == "" {
		c = "the current context"
	} else {
		c = "context " + c
	}
	if n == "" {
		return c
	}
	return c + ", namespace " + n
}

// result is what one kubectl command did.
type result struct {
	Stdout, Stderr string
	Code           int
}

func (r result) err(what string) error {
	msg := strings.TrimSpace(r.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(r.Stdout)
	}
	return fmt.Errorf("%s: kubectl exit %d: %s", what, r.Code, msg)
}

// Run runs kubectl args to completion with stdin. A non-zero exit is a
// result, not an error; the error is for a kubectl that could not be
// run at all.
func (k *Kubectl) run(ctx context.Context, stdin []byte, args ...string) (result, error) {
	argv := k.Argv(args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	procgroup.DieWithParent(cmd)
	err := cmd.Run()
	res := result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ctx.Err() == nil:
		res.Code = ee.ExitCode()
	case ctx.Err() != nil:
		return res, ctx.Err()
	case errors.Is(err, exec.ErrNotFound):
		return res, &location.PrerequisiteError{What: "kubectl was not found", Fix: "install kubectl (https://kubernetes.io/docs/tasks/tools/) and check that `kubectl get pods` works"}
	default:
		return res, err
	}
	return res, nil
}

// must runs kubectl and turns a non-zero exit into an error naming what.
func (k *Kubectl) must(ctx context.Context, what string, stdin []byte, args ...string) (string, error) {
	res, err := k.run(ctx, stdin, args...)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", res.err(what)
	}
	return res.Stdout, nil
}

// exec runs script with sh -c in container c of pod, with stdin.
func (k *Kubectl) exec(ctx context.Context, pod, c, script string, stdin []byte) (result, error) {
	args := []string{"exec", "--quiet", pod, "-c", c}
	if stdin != nil {
		args = append(args, "-i")
	}
	return k.run(ctx, stdin, append(args, "--", "/bin/sh", "-c", script)...)
}

// Output runs kubectl args with stdin and returns its stdout; a
// non-zero exit is an error naming what, with kubectl's message. It is
// how other packages (sandbox/k8s) drive the same kubectl.
func (k *Kubectl) Output(ctx context.Context, what string, stdin []byte, args ...string) (string, error) {
	return k.must(ctx, what, stdin, args...)
}

// Command is an unstarted kubectl args, for a caller that streams its
// output or kills it.
func (k *Kubectl) Command(ctx context.Context, args ...string) *exec.Cmd {
	argv := k.Argv(args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	procgroup.DieWithParent(cmd)
	return cmd
}
