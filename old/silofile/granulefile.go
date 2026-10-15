package silofile

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
)

// pushRepository is a registry repository push_to may name: a host
// (with a port, optionally) and one or more lowercase path components,
// with no tag and no digest, since silo names the image by digest
// itself.
var pushRepository = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?(:[0-9]+)?(/[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*)+$`)

// granulefile checks a granule sandbox's Granulefile with
// Options.CheckGranulefile (design §14, journey 10): it has to be valid,
// and its target stage has to build a granule VM image, since a
// container image cannot boot as a VM.
func (v *validator) granulefile(p, file, target string) {
	if v.opts.CheckGranulefile == nil {
		return
	}
	vm, err := v.opts.CheckGranulefile(file, target)
	switch {
	case errors.Is(err, ErrUnchecked):
		v.warnf(p+".granulefile", "%v", err)
	case err != nil:
		v.errorf(p+".granulefile", "%v", err)
	case !vm:
		v.errorf(p+".granulefile", "its target stage builds a container image, not a granule VM image: end it with VM WRAP or VM PACK (granule docs/granulefile.md §5), or use kind container with build:")
	}
}

// dockerfile refuses a container sandbox's build: file when it is a
// Granulefile that uses granule's VM instruction, which docker build
// cannot read: a VM image is kind granule's, from granulefile:.
func (v *validator) dockerfile(p, file, kind string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	if line := VMInstruction(data); line > 0 {
		v.errorf(p, "line %d is granule's VM instruction: this is a Granulefile, which kind %s cannot build (docker build reads only Dockerfiles); a VM image needs kind granule with granulefile:", line, kind)
	}
}

// VMInstruction returns the line of the first instruction of a
// Dockerfile-format file whose keyword is VM -- the one instruction a
// Granulefile adds to the Dockerfile format (granule
// docs/granulefile.md §3, §5) -- or 0 when there is none. It reads what
// a Dockerfile's lexer does: parser directives, comments, and lines
// continued with the escape character, which an escape directive may
// make a backtick.
func VMInstruction(data []byte) int {
	escape := byte('\\')
	directives := true
	continued := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if directives {
			if d, ok := strings.CutPrefix(line, "#"); ok {
				name, val, ok := strings.Cut(d, "=")
				if ok && strings.EqualFold(strings.TrimSpace(name), "escape") {
					if val = strings.TrimSpace(val); val == "`" {
						escape = '`'
					}
					continue
				}
				if ok && !strings.ContainsAny(strings.TrimSpace(name), " \t") {
					continue
				}
			}
			directives = false
		}
		if line == "" || strings.HasPrefix(line, "#") {
			// Neither ends a continued instruction.
			continue
		}
		start := !continued
		continued = line[len(line)-1] == escape
		if !start {
			continue
		}
		word, _, _ := strings.Cut(strings.Fields(line)[0], string(escape))
		if strings.EqualFold(word, "VM") {
			return n
		}
	}
	return 0
}
