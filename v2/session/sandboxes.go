package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/bwsalmon/silo/v2/credentials"
	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/silofile"
)

func init() { RegisterToolProvider("sandboxes", provideSandboxes) }

// provideSandboxes starts the Silofile's sandboxes (design §7) and
// serves the sandbox tools over them.
func provideSandboxes(ctx context.Context, s *Session) ([]gateway.Tool, Undo, error) {
	if len(s.File.Sandboxes) == 0 {
		return nil, nil, nil
	}
	b, err := s.backend()
	if err != nil {
		return nil, nil, err
	}
	// Recorded before anything is created, so a crash mid-create still
	// leaves silo gc a record to find the containers by.
	if err := s.AddResource(Resource{Kind: KindContainers, Runtime: s.runtime()}); err != nil {
		return nil, nil, err
	}
	// And each cluster's, whose pods carry the session's label as the
	// k8s location's do, so that silo gc deletes them the same way.
	seen := map[silofile.K8sBackend]bool{}
	for _, sc := range s.File.Sandboxes {
		if k := sc.Backend.K8s; k != nil && !seen[*k] {
			seen[*k] = true
			if err := s.AddResource(Resource{Kind: KindPod, Context: k.Context, Namespace: k.Namespace}); err != nil {
				return nil, nil, err
			}
		}
	}
	made := map[string]sandbox.Sandbox{}
	undo := func(ctx context.Context) error {
		var errs []error
		for _, sb := range made {
			errs = append(errs, sb.Destroy(ctx))
		}
		// And anything a cancelled create left behind under the label.
		if found, err := b.Find(ctx, s.ID); err == nil {
			for _, sb := range found {
				errs = append(errs, sb.Destroy(ctx))
			}
		}
		return errors.Join(errs...)
	}
	for _, sc := range s.File.Sandboxes {
		spec, err := sandbox.SpecFor(s.File, sc.Name, s.ID)
		if err != nil {
			return nil, undo, err
		}
		d, err := credentials.ForSandbox(s.File.Credentials, s.Credentials, sc.Name)
		if err != nil {
			return nil, undo, err
		}
		d.Apply(&spec)
		if len(sc.Repos) > 0 {
			spec.PrivateNetwork = s.git.network
		}
		where := sc.Kind
		if sc.Backend.K8s != nil {
			where += " pod"
		}
		s.status("sandbox %s: starting (%s)", sc.Name, where)
		sb, err := b.Create(ctx, spec)
		if err != nil {
			return nil, undo, err
		}
		made[sc.Name] = sb
		if err := d.DeliverFiles(ctx, sb); err != nil {
			return nil, undo, fmt.Errorf("sandbox %s: %w", sc.Name, err)
		}
		if err := s.checkout(ctx, sb, sc); err != nil {
			return nil, undo, fmt.Errorf("sandbox %s: %w", sc.Name, err)
		}
	}
	return gateway.SandboxTools(made), undo, nil
}
