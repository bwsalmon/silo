// Package instructions embeds the shipped default root agent instructions
// (/usr/share/silo/root.md, design §9.1 and Appendix A) and loads overrides
// from /etc/silo/root.md and /etc/silo/root.d/*.md.
package instructions

import (
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultRootMD is the shipped /usr/share/silo/root.md content.
//
//go:embed root.md
var DefaultRootMD string

// Default system paths (design §9.1).
const (
	ShippedRootPath = "/usr/share/silo/root.md"
	EtcRootPath     = "/etc/silo/root.md"
	EtcRootDir      = "/etc/silo/root.d"
)

// Source records one file that contributed to the effective instructions.
type Source struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// EffectiveRoot loads the base root.md (/etc/silo/root.md if present, else
// /usr/share/silo/root.md on disk or embedded DefaultRootMD) and appends any
// *.md files in /etc/silo/root.d in lexical order.
func EffectiveRoot(shippedPath, etcPath, etcDir string) (string, []Source, error) {
	if shippedPath == "" {
		shippedPath = ShippedRootPath
	}
	if etcPath == "" {
		etcPath = EtcRootPath
	}
	if etcDir == "" {
		etcDir = EtcRootDir
	}
	var sources []Source
	if data, err := os.ReadFile(etcPath); err == nil {
		sources = append(sources, Source{Path: etcPath, Content: string(data)})
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	} else if data, err := os.ReadFile(shippedPath); err == nil {
		sources = append(sources, Source{Path: shippedPath, Content: string(data)})
	} else {
		sources = append(sources, Source{Path: ShippedRootPath, Content: DefaultRootMD})
	}

	entries, err := os.ReadDir(etcDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(etcDir, n)
		data, err := os.ReadFile(p)
		if err != nil {
			return "", nil, err
		}
		sources = append(sources, Source{Path: p, Content: string(data)})
	}

	var b strings.Builder
	for i, s := range sources {
		if i > 0 {
			b.WriteString("\n\n<!-- from " + s.Path + " (later files override earlier guidance) -->\n\n")
		}
		b.WriteString(strings.TrimSpace(s.Content))
	}
	b.WriteString("\n")
	return b.String(), sources, nil
}
