package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
)

// GCOptions configure GC.
type GCOptions struct {
	// Live reports whether a session on this machine is still running.
	Live func(id string) bool
	// Origin is this machine's ssh.Origin(), when empty.
	Origin string
	// Keep is a session id never removed: the session running the gc.
	Keep string
	// CheckAge is how old a --dry-run check pod must be before it is
	// removed; 10 minutes when zero.
	CheckAge time.Duration
}

var validID = regexp.MustCompile(`^s-[a-z2-7]{12}$`)

type listed struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name              string            `json:"name"`
			Labels            map[string]string `json:"labels"`
			Annotations       map[string]string `json:"annotations"`
			CreationTimestamp time.Time         `json:"creationTimestamp"`
		} `json:"metadata"`
	} `json:"items"`
}

// GC removes the pods and Secrets silo made from this origin in k's
// namespace whose session is no longer live, and --dry-run check pods
// left behind (design §5.4, §12.6). It returns what it removed, as
// kind/name.
func GC(ctx context.Context, k *Kubectl, opts GCOptions) ([]string, error) {
	origin := opts.Origin
	if origin == "" {
		origin = sshloc.Origin()
	}
	age := opts.CheckAge
	if age == 0 {
		age = 10 * time.Minute
	}
	out, err := k.must(ctx, "listing silo's pods in "+k.Where(), nil, "get", "pods,secrets", "-l", ManagedByLabel+"="+ManagedBy, "-o", "json")
	if err != nil {
		return nil, err
	}
	var l listed
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return nil, fmt.Errorf("listing silo's pods: %w", err)
	}
	var gone []string
	for _, it := range l.Items {
		m := it.Metadata
		if m.Annotations[OriginAnnotation] != origin {
			continue
		}
		id := m.Labels[SessionLabel]
		switch {
		case m.Labels[CheckLabel] == "true":
			if time.Since(m.CreationTimestamp) < age {
				continue
			}
		case validID.MatchString(id):
			if id == opts.Keep || (opts.Live != nil && opts.Live(id)) {
				continue
			}
		default:
			continue
		}
		gone = append(gone, strings.ToLower(it.Kind)+"/"+m.Name)
	}
	if len(gone) == 0 {
		return nil, nil
	}
	_, err = k.must(ctx, "deleting stale pods in "+k.Where(), nil, append(append([]string{"delete"}, gone...), "--ignore-not-found", "--wait=false")...)
	if err != nil {
		return nil, errors.Join(err)
	}
	return gone, nil
}
