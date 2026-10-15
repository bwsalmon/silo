package granule

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var pushedDigest = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)

// Push pushes the image the daemon holds as local to repo, a registry
// repository (push_to:), under local's own tag, and returns it as
// repo@<digest>: what a remote backend's nodes pull (design §14,
// journey 10), named by content so that a later push to the same tag
// cannot change what a running sandbox was made from.
func (b *Backend) Push(ctx context.Context, local, repo string) (string, error) {
	tag := local[strings.LastIndex(local, ":")+1:]
	if i := strings.LastIndex(local, ":"); i < 0 || strings.Contains(local[i:], "/") {
		tag = "latest"
	}
	remote := repo + ":" + tag
	if _, err := run(ctx, b.docker(), nil, "tag", local, remote); err != nil {
		return "", err
	}
	out, err := run(ctx, b.docker(), nil, "push", remote)
	if err != nil {
		return "", fmt.Errorf("pushing to %s: %w", repo, err)
	}
	// The registry's digest of what was pushed, which docker prints. The
	// daemon's RepoDigests can name the source's multi-platform index
	// instead, which the registry does not have.
	if m := pushedDigest.FindStringSubmatch(string(out)); m != nil {
		return repo + "@" + m[1], nil
	}
	return "", fmt.Errorf("pushed %s, but the daemon records no digest for it in %s", remote, repo)
}
