package credentials

import (
	"context"
	"fmt"
	"sort"

	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/silofile"
)

// SandboxDelivery is what one sandbox receives: variables set in its
// environment when it is created ({sandbox: x, env: NAME}), and files
// written into it once it runs ({sandbox: x, file: /path}).
type SandboxDelivery struct {
	Env   map[string]string
	Files []File
}

// File is one credential file to deliver.
type File struct {
	Credential string
	Path       string
	Data       []byte
}

// ForSandbox returns what the sandbox named name receives. A credential
// silo keeps for itself ({ambient: gcloud}) is refused anywhere.
func ForSandbox(creds []silofile.Credential, values map[string]Value, name string) (SandboxDelivery, error) {
	d := SandboxDelivery{Env: map[string]string{}}
	for _, c := range creds {
		for _, to := range c.To {
			if to.Sandbox != name {
				continue
			}
			v, ok := values[c.Name]
			if !ok {
				return SandboxDelivery{}, fmt.Errorf("credential %q is not resolved", c.Name)
			}
			if v.SilosOwn {
				return SandboxDelivery{}, fmt.Errorf("credential %q comes from {ambient: gcloud}, which is silo's own and is never delivered (design §8.1)", c.Name)
			}
			switch {
			case to.Env != "":
				d.Env[to.Env] = v.Secret
			case to.File != "":
				d.Files = append(d.Files, File{Credential: c.Name, Path: to.File, Data: []byte(v.Secret)})
			default:
				return SandboxDelivery{}, fmt.Errorf("credential %q: a sandbox destination needs env or file", c.Name)
			}
		}
	}
	sort.SliceStable(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	return d, nil
}

// Apply adds the delivery's variables to spec's environment. A
// credential wins over a Silofile env entry of the same name.
func (d SandboxDelivery) Apply(spec *sandbox.SandboxSpec) {
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	for k, v := range d.Env {
		spec.Env[k] = v
	}
}

// DeliverFiles writes the delivery's files into a running sandbox, each
// mode 0400 and replaced atomically.
func (d SandboxDelivery) DeliverFiles(ctx context.Context, sb sandbox.Sandbox) error {
	for _, f := range d.Files {
		if err := sb.DeliverFile(ctx, f.Path, f.Data); err != nil {
			return fmt.Errorf("credential %q: %w", f.Credential, err)
		}
	}
	return nil
}
