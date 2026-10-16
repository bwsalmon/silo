package granule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/old/internal/lockfile"
	"github.com/bwsalmon/silo/old/internal/procgroup"
	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/silofile"
)

// BuiltRepository is the local image repository Granulefiles are built
// into, each under a tag that is its CacheKey: a key whose tag the
// daemon already has is built, and a second run boots it rather than
// building again. Local, and not a name any registry serves.
const BuiltRepository = "silo-granulefile"

// CheckTimeout bounds `granule granulefile check`, which reads one file
// and builds nothing.
const CheckTimeout = 30 * time.Second

// CacheKey is the content key of a Granulefile build (design §14,
// journey 10): SHA-256 over the Granulefile, its build context (the
// Granulefile's directory, which is what the build sends), the target
// and the build args. Within the context it reads every file's
// relative path, executable bit and content and every symlink's
// target, in lexical order; it skips granule's own scratch directories
// (.granule-build-*) and silo's .silo, which change from run to run
// without changing what is built. It reads more than a .dockerignore
// would send, which costs at worst a rebuild, never a stale image.
func CacheKey(granulefile, target string, args map[string]string) (string, error) {
	h := sha256.New()
	field := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(h, "%d:%s", len(p), p)
		}
	}
	field("silo-granulefile/1")
	content, err := os.ReadFile(granulefile)
	if err != nil {
		return "", err
	}
	field("granulefile", string(content), "target", target)
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		field("arg", k, args[k])
	}
	dir := filepath.Dir(granulefile)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == filepath.Dir(lockfile.Path) || strings.HasPrefix(d.Name(), ".granule-build-") {
				return filepath.SkipDir
			}
			field("dir", rel)
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			field("symlink", rel, link)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			exe := "0"
			if info.Mode()&0o111 != 0 {
				exe = "1"
			}
			field("file", rel, exe, strconv.FormatInt(info.Size(), 10))
			if err := hashFile(h, path); err != nil {
				return err
			}
		default:
			field("other", rel, d.Type().String())
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("reading the build context %s: %w", dir, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(h hash.Hash, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(h, f)
	return err
}

// BuiltTag is the image a CacheKey's build is kept under.
func BuiltTag(key string) string {
	return BuiltRepository + ":" + strings.TrimPrefix(key, "sha256:")
}

// Check runs `granule granulefile check` on a Granulefile, with its
// directory as the context, and reports whether its target stage (the
// last, when target is empty) builds a granule VM image or a container
// image -- which granule's plan says ("target: ..., which produces a
// granule VM image"), so silo reads no Granulefile grammar of its own.
// It builds nothing and needs neither Docker nor KVM.
func (b *Backend) Check(ctx context.Context, granulefile, target string) (vm bool, err error) {
	args := []string{"granulefile", "check", "-f", granulefile}
	if target != "" {
		args = append(args, "-target", target)
	}
	out, err := run(ctx, b.granule(), nil, append(args, filepath.Dir(granulefile))...)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "target: ") {
			continue
		}
		switch {
		case strings.HasSuffix(line, "which produces a granule VM image"):
			return true, nil
		case strings.HasSuffix(line, "which produces an OCI container image"):
			return false, nil
		}
	}
	return false, fmt.Errorf("granule granulefile check did not say what %s builds:\n%s", filepath.Base(granulefile), strings.TrimSpace(string(out)))
}

// CheckGranulefile is silofile.Options.CheckGranulefile with the
// granule SILO_GRANULE names, or granule on PATH: without one, the
// Granulefile is left unchecked, which validation reports as a warning.
func CheckGranulefile(file, target string) (bool, error) {
	bin := os.Getenv(BinaryEnv)
	if bin == "" {
		bin = "granule"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return false, fmt.Errorf("%w: granule is not installed, so granule granulefile check could not run (go build ./cmd/granule in gke-labs/granule, or set %s)", silofile.ErrUnchecked, BinaryEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), CheckTimeout)
	defer cancel()
	return (&Backend{Granule: bin}).Check(ctx, file, target)
}

// Cached returns the image id of a Granulefile's build already kept
// under its key's tag, or "" when there is none.
func (b *Backend) Cached(ctx context.Context, key string) string {
	out, err := run(ctx, b.docker(), nil, "image", "inspect", "--format", "{{.Id}}", BuiltTag(key))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// BuildArgs returns the `granule granulefile build` arguments that
// build spec's Granulefile into tag.
func BuildArgs(spec sandbox.SandboxSpec, tag string) []string {
	args := []string{"granulefile", "build", "-f", spec.Build, "-t", tag}
	if spec.Target != "" {
		args = append(args, "-target", spec.Target)
	}
	keys := make([]string, 0, len(spec.BuildArgs))
	for k := range spec.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-build-arg", k+"="+spec.BuildArgs[k])
	}
	return append(args, filepath.Dir(spec.Build))
}

// buildOutputLines is how much of a failed build's output its error
// carries.
const buildOutputLines = 40

// BuildImage returns spec's Granulefile built into a granule VM image:
// the one kept under its CacheKey's tag when the daemon has it, or
// else a fresh `granule granulefile build` into that tag. A Granulefile
// whose target builds a container image is refused before anything is
// built. The Image's Ref is the Granulefile, its Digest the image id
// and its Key the CacheKey; the image is booted by its tag.
func (b *Backend) BuildImage(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.Image, string, error) {
	key, err := CacheKey(spec.Build, spec.Target, spec.BuildArgs)
	if err != nil {
		return sandbox.Image{}, "", fmt.Errorf("sandbox %q: granulefile: %w", spec.Name, err)
	}
	tag := BuiltTag(key)
	img := sandbox.Image{Ref: spec.Build, Key: key}
	if id := b.Cached(ctx, key); id != "" {
		img.Digest = id
		return img, tag, nil
	}
	vm, err := b.Check(ctx, spec.Build, spec.Target)
	if err != nil {
		return sandbox.Image{}, "", fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	if !vm {
		return sandbox.Image{}, "", fmt.Errorf("sandbox %q: %s's target stage builds a container image, not a granule VM image: kind granule boots a VM", spec.Name, filepath.Base(spec.Build))
	}
	// granule builds with the docker on PATH; b.Docker is not passed on,
	// since granulefile build has no flag for it.
	cmd := exec.CommandContext(ctx, b.granule(), BuildArgs(spec, tag)...)
	procgroup.Prepare(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return sandbox.Image{}, "", fmt.Errorf("sandbox %q: granule granulefile build %s: %w\n%s", spec.Name, filepath.Base(spec.Build), err, lastLines(string(out), buildOutputLines))
	}
	id := b.Cached(ctx, key)
	if id == "" {
		return sandbox.Image{}, "", fmt.Errorf("sandbox %q: granule granulefile build %s left no image %s", spec.Name, filepath.Base(spec.Build), tag)
	}
	img.Digest = id
	return img, tag, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"..."}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}
