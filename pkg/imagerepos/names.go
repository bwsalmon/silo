package imagerepos

import (
	"regexp"
	"strings"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Host is the registry's name in silo-net (local-registry.md, "The
// address", call L56).
const Host = "registry.silo.internal"

// IDPrefix marks a name's first component as a silo by id
// (local-registry.md, "Repository names", call L57). A silo name can't
// contain "_", so no silo path starts with it.
const IDPrefix = "id_"

var (
	nameRE = regexp.MustCompile(`^[a-z0-9]+((_|__|-+)[a-z0-9]+)*$`)
	tagRE  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	// The upstream form's components: the OCI name grammar's, dots
	// included.
	upstreamRE = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	hostRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)
	digestRE   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$|^sha512:[a-f0-9]{128}$`)
	routeRE    = regexp.MustCompile(`^/v2/(.+)/(manifests/[^/]+|blobs/uploads/?[^/]*|blobs/[^/]+|tags/list|referrers/[^/]+)$`)
)

// SplitPath splits a distribution API path under /v2/ into the
// repository name, for ParseName, and what follows it: manifests/<ref>,
// blobs/<digest>, blobs/uploads/[<id>], tags/list or referrers/<digest>.
// It reports false for any other path, /v2/ and /v2/_catalog included.
func SplitPath(path string) (name, rest string, ok bool) {
	m := routeRE.FindStringSubmatch(path)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// ValidName reports whether name may name an image repo: one
// component of 1 to 63 characters matching the OCI name grammar
// without its ".", and not starting with id_ (call L57). A dot would
// make a root repo's name look like an upstream.
func ValidName(name string) bool {
	return len(name) <= 63 && nameRE.MatchString(name) && !strings.HasPrefix(name, IDPrefix)
}

// ValidTag reports whether tag is an OCI tag (distribution-spec,
// "Pulling manifests"): up to 128 characters of [a-zA-Z0-9._-], not
// starting with "." or "-".
func ValidTag(tag string) bool { return tagRE.MatchString(tag) }

// ValidDigest reports whether d is a sha256 or sha512 digest in the
// form the registry stores: algorithm, colon, lower-case hex.
func ValidDigest(d string) bool { return digestRE.MatchString(d) }

// Name is a registry repository name (the part of a reference between
// the host and the tag or digest), as ParseName splits it. Exactly one
// of Upstream, SiloID and the path form is set: Upstream names the
// pull-through cache's upstream; otherwise the name is a local repo,
// Repo of the silo named by SiloID or by SiloPath ("/" for the root).
type Name struct {
	Upstream     string // a configured upstream's host, such as docker.io
	UpstreamRepo string // the repo on it, such as library/alpine
	SiloPath     string // "/team-a/x", or "/" for the root's repos
	SiloID       string
	Repo         string
}

// Local reports whether n names a local repo rather than the cache.
func (n Name) Local() bool { return n.Upstream == "" }

// String writes n back as the name it was parsed from, by path or by
// id; ParseName(n.String()) is n.
func (n Name) String() string {
	switch {
	case n.Upstream != "":
		return n.Upstream + "/" + n.UpstreamRepo
	case n.SiloID != "":
		return IDPrefix + n.SiloID + "/" + n.Repo
	case n.SiloPath == "/":
		return n.Repo
	}
	return strings.TrimPrefix(n.SiloPath, "/") + "/" + n.Repo
}

// ParseName splits a repository name by its first component (call
// L57): one with a "." is an upstream of the cache, id_<silo id> is a
// silo by id, and anything else is a silo's path whose last component
// is the repo. ns is a mirror request's ns query parameter: one naming
// an upstream other than Host selects the cache for the whole name.
// It reports false for a name no form takes. Docker Hub's short names
// are normalised as Docker does them: docker.io/alpine is
// docker.io/library/alpine, and index.docker.io and
// registry-1.docker.io are docker.io.
func ParseName(name, ns string) (Name, bool) {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return Name{}, false
	}
	comps := strings.Split(name, "/")
	if ns != "" && ns != Host {
		if !hostRE.MatchString(ns) {
			return Name{}, false
		}
		return upstream(ns, comps)
	}
	first := comps[0]
	switch {
	case strings.Contains(first, "."):
		if !hostRE.MatchString(first) {
			return Name{}, false
		}
		return upstream(first, comps[1:])
	case strings.HasPrefix(first, IDPrefix):
		id := strings.TrimPrefix(first, IDPrefix)
		if len(comps) != 2 || !ids.Valid(id) || !ValidName(comps[1]) {
			return Name{}, false
		}
		return Name{SiloID: id, Repo: comps[1]}, true
	}
	repo := comps[len(comps)-1]
	if !ValidName(repo) {
		return Name{}, false
	}
	for _, c := range comps[:len(comps)-1] {
		if !tree.ValidName(c) {
			return Name{}, false
		}
	}
	return Name{SiloPath: "/" + strings.Join(comps[:len(comps)-1], "/"), Repo: repo}, true
}

// dockerHub are the names Docker gives Docker Hub.
var dockerHub = map[string]bool{"docker.io": true, "index.docker.io": true, "registry-1.docker.io": true}

func upstream(host string, comps []string) (Name, bool) {
	if len(comps) == 0 {
		return Name{}, false
	}
	for _, c := range comps {
		if !upstreamRE.MatchString(c) {
			return Name{}, false
		}
	}
	if dockerHub[host] {
		host = "docker.io"
		if len(comps) == 1 {
			comps = append([]string{"library"}, comps...)
		}
	}
	n := Name{Upstream: host, UpstreamRepo: strings.Join(comps, "/")}
	if len(n.String()) > 255 {
		return Name{}, false
	}
	return n, true
}

// Ref is the name a caller pushes to and pulls from silo siloPath's
// repo: registry.silo.internal/<silo path>/<repo>.
func Ref(siloPath, repo string) string {
	return Host + "/" + Name{SiloPath: siloPath, Repo: repo}.String()
}

// RefByID is the same repo's name by its silo's id, which survives a
// rename or move: registry.silo.internal/id_<silo id>/<repo>.
func RefByID(siloID, repo string) string {
	return Host + "/" + Name{SiloID: siloID, Repo: repo}.String()
}
