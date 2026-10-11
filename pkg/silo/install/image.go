package install

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The image step's repository, and the release images it copies from.
const (
	// RepositoryName is the Artifact Registry repository in the home
	// region that the control plane's VMs pull silod from.
	RepositoryName = "silo"
	// ImageName is silod's image in it.
	ImageName = "silod"
	// ReleaseRepository is where silod's release images are published:
	// --version v1.2.3 deploys ReleaseRepository:v1.2.3, copied into the
	// project by digest. (Nothing publishes them yet; until something
	// does, build one with make silod-image and give --image.)
	ReleaseRepository = "ghcr.io/bwsalmon/silod"
)

// SourceImage is the image install deploys, as the installer named it:
// --image, or the release's tag.
func SourceImage(cfg *Config) string {
	if cfg.Image != "" {
		return cfg.Image
	}
	return ReleaseRepository + ":" + cfg.Version
}

// ProjectRepository is the project's copy of silod's image, without a
// digest.
func ProjectRepository(cfg *Config) string {
	return fmt.Sprintf("%s-docker.pkg.dev/%s/%s/%s", cfg.HomeRegion(), cfg.Project, RepositoryName, ImageName)
}

// HarnessImageName is the harness image's name in the project's
// repository (cmd/silod/Dockerfile.harness): what agents' session
// containers run, with both adapters' CLIs pinned.
const HarnessImageName = ImageName + "-harness"

// SourceHarnessImage is the harness image install deploys with
// --agent-sessions: --harness-image, or the release's, beside silod's.
func SourceHarnessImage(cfg *Config) string {
	if cfg.HarnessImage != "" {
		return cfg.HarnessImage
	}
	return ReleaseRepository + "-harness:" + cfg.Version
}

// ProjectHarnessRepository is the project's copy of the harness image,
// without a digest.
func ProjectHarnessRepository(cfg *Config) string { return ProjectRepository(cfg) + "-harness" }

// DeployedHarnessImage is the harness image the control VMs run, by
// digest; "" while the digest isn't known.
func DeployedHarnessImage(cfg *Config, digest string) string {
	if digest == "" {
		return ""
	}
	return ProjectHarnessRepository(cfg) + "@" + digest
}

// HarnessDigest is the harness image's digest, resolved once, as
// ImageDigest is silod's.
func (e *Env) HarnessDigest(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.harnessDigest != "" {
		return e.harnessDigest, nil
	}
	src := SourceHarnessImage(&e.Config)
	if i := strings.Index(src, "@"); i >= 0 {
		e.harnessDigest = src[i+1:]
		return e.harnessDigest, nil
	}
	if e.Cloud.Registry == nil {
		return "", fmt.Errorf("no registry to resolve %s in", src)
	}
	d, err := e.Cloud.Registry.Resolve(ctx, src)
	if err != nil {
		return "", fmt.Errorf("resolving the harness image %s: %w", src, err)
	}
	e.harnessDigest = d
	return d, nil
}

// DeployedImage is the image the control plane's VMs run: the
// project's copy, by digest.
func DeployedImage(cfg *Config, digest string) string {
	return ProjectRepository(cfg) + "@" + digest
}

// sourceRepository is ref without its tag or digest.
func sourceRepository(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// ImageDigest is the digest of the image this install deploys: --image's
// own, or the release tag's, read once from its registry. The image,
// migrate and control steps all deploy this one digest.
func (e *Env) ImageDigest(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.digest != "" {
		return e.digest, nil
	}
	src := SourceImage(&e.Config)
	if i := strings.Index(src, "@"); i >= 0 {
		e.digest = src[i+1:]
		return e.digest, nil
	}
	if e.Cloud.Registry == nil {
		return "", fmt.Errorf("no registry to resolve %s in", src)
	}
	d, err := e.Cloud.Registry.Resolve(ctx, src)
	if err != nil {
		return "", fmt.Errorf("resolving silod's release image %s: %w", src, err)
	}
	e.digest = d
	return d, nil
}

// Image is step 11, image: the Artifact Registry repository silo in the
// home region, and the silod image copied into it by digest (call 19),
// so that the control plane pulls from the project and never from the
// release registry, and runs exactly the digest install resolved.
type Image struct{ Env *Env }

// ImageObserved is the repository, and whether it holds the digest.
type ImageObserved struct {
	Source string `json:"source"`
	Digest string `json:"digest,omitempty"`
	// ResolveError is why the source's digest couldn't be read.
	ResolveError string      `json:"resolveError,omitempty"`
	Enabled      bool        `json:"enabled"`
	Repository   *Repository `json:"repository,omitempty"`
	Copied       bool        `json:"copied,omitempty"`
	// The harness image's, with --agent-sessions.
	HarnessSource       string `json:"harnessSource,omitempty"`
	HarnessDigest       string `json:"harnessDigest,omitempty"`
	HarnessResolveError string `json:"harnessResolveError,omitempty"`
	HarnessCopied       bool   `json:"harnessCopied,omitempty"`
}

func (s *Image) Name() string          { return "image" }
func (s *Image) NewObserved() Observed { return &ImageObserved{} }

func (s *Image) Observe(ctx context.Context) (Observed, error) {
	cfg := &s.Env.Config
	obs := &ImageObserved{Source: SourceImage(cfg)}
	d, err := s.Env.ImageDigest(ctx)
	if err != nil {
		obs.ResolveError = err.Error()
	}
	obs.Digest = d
	if cfg.AgentSessions > 0 {
		obs.HarnessSource = SourceHarnessImage(cfg)
		if obs.HarnessDigest, err = s.Env.HarnessDigest(ctx); err != nil {
			obs.HarnessResolveError = err.Error()
		}
	}
	if obs.Enabled, err = s.Env.Enabled(ctx, "artifactregistry.googleapis.com"); err != nil || !obs.Enabled {
		return obs, err
	}
	r, err := s.Env.Cloud.Artifacts.GetRepository(ctx, cfg.Project, cfg.HomeRegion(), RepositoryName)
	if IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Repository = r
	if d == "" {
		return obs, nil
	}
	if obs.Copied, err = s.Env.Cloud.Registry.Has(ctx, ProjectRepository(cfg), d); err != nil {
		return nil, fmt.Errorf("reading %s: %w", ProjectRepository(cfg), err)
	}
	if obs.HarnessDigest != "" {
		if obs.HarnessCopied, err = s.Env.Cloud.Registry.Has(ctx, ProjectHarnessRepository(cfg), obs.HarnessDigest); err != nil {
			return nil, fmt.Errorf("reading %s: %w", ProjectHarnessRepository(cfg), err)
		}
	}
	return obs, nil
}

func (s *Image) Plan(o Observed) []Change {
	obs := o.(*ImageObserved)
	cfg := &s.Env.Config
	if obs.ResolveError != "" {
		return []Change{{Action: ActionFail, Object: obs.Source, Detail: obs.ResolveError,
			Why: "install deploys silod's image by digest",
			Fix: "check --version, or build an image (make silod-image) and give --image <repository>@sha256:<digest>"}}
	}
	if obs.HarnessResolveError != "" {
		return []Change{{Action: ActionFail, Object: obs.HarnessSource, Detail: obs.HarnessResolveError,
			Why: "with --agent-sessions, the control VMs run agents' harnesses from the harness image, by digest",
			Fix: "check --version, or build one (make silod-harness-image) and give --harness-image <repository>@sha256:<digest>"}}
	}
	short := obs.Digest
	if len(short) > len("sha256:")+12 {
		short = short[:len("sha256:")+12]
	}
	var out []Change
	if obs.Repository == nil {
		out = append(out, Change{Action: ActionCreate, Object: RepositoryName, Detail: "docker repository in " + cfg.HomeRegion(),
			Estimate: 10 * time.Second})
	} else if obs.Repository.Labels[LabelKey] != s.Env.ID {
		return []Change{notOurs(s.Env, RepositoryName, "the control plane pulls silod from install's own repository",
			fmt.Sprintf("gcloud artifacts repositories delete %s --location %s", RepositoryName, cfg.HomeRegion()))}
	} else if obs.Repository.Format != "DOCKER" {
		return []Change{{Action: ActionFail, Object: RepositoryName, Detail: "differs: it is a " + obs.Repository.Format + " repository",
			Why: "a repository's format can't change in place",
			Fix: fmt.Sprintf("gcloud artifacts repositories delete %s --location %s, then install again", RepositoryName, cfg.HomeRegion())}}
	}
	if !obs.Copied {
		out = append(out, Change{Action: ActionApply, Object: ImageName + "@" + short, Detail: "copy from " + sourceRepository(obs.Source),
			Estimate: time.Minute})
	}
	if obs.HarnessDigest != "" && !obs.HarnessCopied {
		out = append(out, Change{Action: ActionApply, Object: HarnessImageName + "@" + shortDigest(obs.HarnessDigest),
			Detail: "copy from " + sourceRepository(obs.HarnessSource), Estimate: time.Minute})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: RepositoryName, Detail: ImageName + "@" + short}}
	}
	return out
}

// PlanRemove deletes the repository, and the image copies in it.
func (s *Image) PlanRemove(o Observed) []Change {
	obs := o.(*ImageObserved)
	if obs.Repository == nil {
		return []Change{{Action: ActionOK, Detail: "no repository " + RepositoryName}}
	}
	if obs.Repository.Labels[LabelKey] != s.Env.ID {
		return []Change{{Action: ActionOK, Object: RepositoryName, Detail: "is not this install's; left alone"}}
	}
	return []Change{{Action: ActionDelete, Object: RepositoryName, Detail: "and its silod images"}}
}

func (s *Image) Apply(ctx context.Context, changes []Change) error {
	cfg := &s.Env.Config
	for _, ch := range changes {
		switch ch.Action {
		case ActionDelete:
			err := s.Env.Cloud.Artifacts.DeleteRepository(ctx, cfg.Project, cfg.HomeRegion(), RepositoryName)
			if err != nil && !IsNotFound(err) {
				return fmt.Errorf("deleting repository %s: %w", RepositoryName, err)
			}
		case ActionCreate:
			err := s.Env.Cloud.Artifacts.CreateRepository(ctx, cfg.Project, cfg.HomeRegion(),
				&Repository{Name: RepositoryName, Format: "DOCKER", Labels: map[string]string{LabelKey: s.Env.ID}})
			if err != nil && !IsAlreadyExists(err) {
				return fmt.Errorf("creating repository %s: %w", RepositoryName, err)
			}
		case ActionApply:
			if strings.HasPrefix(ch.Object, HarnessImageName+"@") {
				d, err := s.Env.HarnessDigest(ctx)
				if err != nil {
					return err
				}
				src := sourceRepository(SourceHarnessImage(cfg))
				if err := s.Env.Cloud.Registry.Copy(ctx, src, d, ProjectHarnessRepository(cfg)); err != nil {
					return fmt.Errorf("copying %s@%s into %s: %w", src, d, ProjectHarnessRepository(cfg), err)
				}
				continue
			}
			d, err := s.Env.ImageDigest(ctx)
			if err != nil {
				return err
			}
			if err := s.Env.Cloud.Registry.Copy(ctx, sourceRepository(SourceImage(cfg)), d, ProjectRepository(cfg)); err != nil {
				return fmt.Errorf("copying %s@%s into %s: %w", sourceRepository(SourceImage(cfg)), d, ProjectRepository(cfg), err)
			}
		}
	}
	return nil
}
