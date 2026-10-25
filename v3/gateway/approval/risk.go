package approval

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
)

// Flag is a risk silo computed from the batch itself, shown before anything
// the agent wrote (design §6.3). Where says which file write or step raised
// it, so the UI can link to it.
type Flag struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Where   string `json:"where"`
	// Blocking flags can never be matched by a standing approval.
	Blocking bool `json:"blocking,omitempty"`
}

// Flag codes.
const (
	FlagRootInstructions = "root-instructions" // edits the root agent's own instructions
	FlagSiloConfig       = "silo-config"       // touches /etc/silo or silo's units, state or audit log
	FlagIndirection      = "indirection"       // sh -c, python -c, perl -e, ...
	FlagDownloadExec     = "download-exec"     // curl … | sh, wget + chmod +x
	FlagDebInstall       = "deb-install"       // installs a local .deb
	FlagFirewall         = "firewall"          // nftables, iptables, ufw, firewalld
	FlagSudoersPAM       = "sudoers-pam"       // sudoers or PAM
	FlagCredentials      = "credentials"       // /etc/shadow, SSH keys, authorized_keys
	FlagEnableOwnUnit    = "enable-own-unit"   // systemctl enable of a unit file this batch writes
	FlagAptSource        = "apt-source"        // adds an apt source or key
	FlagUsersGroups      = "users-groups"      // changes users or groups
	FlagCloudCLI         = "cloud-cli"         // gcloud, aws, az, ...
	FlagEnvLoader        = "env-loader"        // a step sets PATH or LD_*
	FlagUnusualChars     = "unusual-chars"     // control, bidi or non-ASCII characters
	FlagExecAttachment   = "exec-attachment"   // runs a script from the batch's attachments
)

// RootInstructionsMessage is the exact text of the root-instructions flag.
const RootInstructionsMessage = "changes the root agent's own instructions"

// isRootInstructionsPath reports whether p is under /etc/silo/root* or
// /usr/share/silo/ (design §9.2).
func isRootInstructionsPath(p string) bool {
	return strings.HasPrefix(p, "/etc/silo/root") || p == "/usr/share/silo" || strings.HasPrefix(p, "/usr/share/silo/")
}

type pathRule struct {
	code, msg string
	match     func(p string) bool
}

func prefix(ps ...string) func(string) bool {
	return func(p string) bool {
		for _, q := range ps {
			if p == strings.TrimSuffix(q, "/") || strings.HasPrefix(p, q) {
				return true
			}
		}
		return false
	}
}

var pathRules = []pathRule{
	{FlagRootInstructions, RootInstructionsMessage, isRootInstructionsPath},
	{FlagSiloConfig, "touches silo's own configuration, units, state or audit log", func(p string) bool {
		return !isRootInstructionsPath(p) && (prefix("/etc/silo/", "/var/lib/silo/gateway/", "/var/log/silo/", "/run/silo/")(p) ||
			siloUnitRe.MatchString(p))
	}},
	{FlagSudoersPAM, "edits sudoers or PAM", prefix("/etc/sudoers", "/etc/pam.d/", "/etc/pam.conf", "/etc/security/")},
	{FlagFirewall, "changes the firewall", prefix("/etc/nftables", "/etc/iptables/", "/etc/ufw/", "/etc/firewalld/", "/etc/default/ufw")},
	{FlagCredentials, "touches credentials: shadow passwords, SSH keys or authorized_keys", func(p string) bool {
		return prefix("/etc/shadow", "/etc/gshadow", "/etc/ssh/", "/root/.ssh/")(p) || strings.Contains(p, "/.ssh/") || path.Base(p) == "authorized_keys"
	}},
	{FlagUsersGroups, "changes users or groups", prefix("/etc/passwd", "/etc/group", "/etc/subuid", "/etc/subgid")},
	{FlagAptSource, "adds or changes an apt source or key", prefix("/etc/apt/sources.list", "/etc/apt/trusted.gpg", "/etc/apt/keyrings/", "/usr/share/keyrings/", "/etc/apt/preferences")},
}

// blockingCodes can never be auto-approved (design §7.5).
var blockingCodes = map[string]bool{
	FlagRootInstructions: true, FlagSiloConfig: true, FlagSudoersPAM: true, FlagFirewall: true, FlagCredentials: true,
}

var (
	shells      = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "ash": true, "busybox": true, "fish": true}
	interpExec  = map[string]string{"python": "-c", "python3": "-c", "python2": "-c", "perl": "-e", "ruby": "-e", "node": "-e", "nodejs": "-e", "php": "-r", "lua": "-e"}
	firewallCmd = map[string]bool{"nft": true, "iptables": true, "ip6tables": true, "iptables-restore": true, "ip6tables-restore": true, "iptables-nft": true, "iptables-legacy": true, "ebtables": true, "ufw": true, "firewall-cmd": true}
	userCmd     = map[string]bool{"useradd": true, "adduser": true, "userdel": true, "deluser": true, "usermod": true, "groupadd": true, "addgroup": true, "groupdel": true, "delgroup": true, "groupmod": true, "gpasswd": true, "chpasswd": true, "passwd": true, "newusers": true}
	cloudCmd    = map[string]bool{"gcloud": true, "gsutil": true, "bq": true, "aws": true, "az": true, "doctl": true, "oci": true}
	sudoersCmd  = map[string]bool{"visudo": true, "pam-auth-update": true}
	aptSrcCmd   = map[string]bool{"add-apt-repository": true, "apt-key": true}
	pipeToShRe  = regexp.MustCompile(`(curl|wget|fetch)\b[^|;&]*\|\s*(sudo\s+)?(env\s+\S+\s+)*(ba|da|z|k|a)?sh\b`)
	dlChmodRe   = regexp.MustCompile(`(curl|wget)\b[\s\S]*chmod\s+[^\n]*\+?x`)
	debRe       = regexp.MustCompile(`\.deb$`)
	siloUnitRe  = regexp.MustCompile(`^/(etc|lib|usr/lib)/systemd/system/silo[-.@]`)
)

// Flags computes the risk flags of a bound batch. attachments holds the
// attachments' content by name, so scripts can be scanned too.
func Flags(bd *Bound, attachments map[string][]byte) []Flag {
	var out []Flag
	add := func(code, msg, where string) {
		for _, f := range out {
			if f.Code == code && f.Where == where {
				return
			}
		}
		out = append(out, Flag{Code: code, Message: msg, Where: where, Blocking: blockingCodes[code]})
	}
	checkPath := func(p, where string) {
		for _, r := range pathRules {
			if r.match(p) {
				add(r.code, r.msg, where)
			}
		}
	}
	checkText := func(s, where string) {
		if pipeToShRe.MatchString(s) {
			add(FlagDownloadExec, "downloads and executes (curl … | sh)", where)
		} else if dlChmodRe.MatchString(s) {
			add(FlagDownloadExec, "downloads a file and makes it executable", where)
		}
		for _, f := range fields(s) {
			if strings.HasPrefix(f, "/") {
				checkPath(path.Clean(f), where)
			}
		}
	}
	checkChars := func(s, where string) {
		if odd := unusualChars(s); odd != "" {
			add(FlagUnusualChars, "contains "+odd+" characters, shown escaped", where)
		}
	}

	written := map[string]bool{}
	for i, f := range bd.Files {
		where := fmt.Sprintf("files[%d] %s", i, f.Path)
		checkPath(f.Path, where)
		checkChars(f.Path, where)
		if isUnitPath(f.Path) {
			written[path.Base(f.Path)] = true
		}
	}
	for _, a := range bd.Attachments {
		if c, ok := attachments[a.Name]; ok && isText(c) {
			checkText(string(c), "attachment "+a.Name)
		}
	}
	for i, s := range bd.Steps {
		where := fmt.Sprintf("steps[%d]", i)
		cmd := s.Command
		base := path.Base(cmd[0])
		if name, ok := strings.CutPrefix(cmd[0], AttachmentPrefix); ok {
			add(FlagExecAttachment, "runs the attached script "+name+" as root", where)
			base = name
		}
		for _, a := range cmd {
			checkChars(a, where)
		}
		for k, v := range s.Env {
			checkChars(k+"="+v, where)
			if k == "PATH" || strings.HasPrefix(k, "LD_") {
				add(FlagEnvLoader, "sets "+k+", which changes what the step loads", where)
			}
		}
		args := cmd[1:]
		if base == "env" {
			// env [-i] [VAR=x ...] cmd args...
			for len(args) > 0 && (strings.HasPrefix(args[0], "-") || strings.Contains(args[0], "=")) {
				args = args[1:]
			}
			if len(args) > 0 {
				base, args = path.Base(args[0]), args[1:]
			}
		}
		switch {
		case shells[base] && hasFlag(args, "-c"):
			add(FlagIndirection, base+" -c runs a shell string; what it does is not visible as argv", where)
		case interpExec[base] != "" && hasFlag(args, interpExec[base]):
			add(FlagIndirection, base+" "+interpExec[base]+" runs inline code", where)
		case shells[base] || strings.HasPrefix(base, "python") || interpExec[base] != "":
			add(FlagIndirection, base+" interprets a script", where)
		}
		joined := strings.Join(cmd, " ")
		checkText(joined, where)
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "--"); ok {
				if _, v, ok := strings.Cut(p, "="); ok {
					a = v
				}
			}
			if strings.HasPrefix(a, "/") {
				checkPath(path.Clean(a), where)
			}
		}
		switch {
		case base == "dpkg" && (hasFlag(args, "-i") || hasFlag(args, "--install")),
			(base == "apt" || base == "apt-get" || base == "gdebi") && anyMatch(args, debRe):
			add(FlagDebInstall, "installs a local .deb package, which bypasses the distribution archives", where)
		case firewallCmd[base]:
			add(FlagFirewall, "changes the firewall", where)
		case sudoersCmd[base]:
			add(FlagSudoersPAM, "edits sudoers or PAM", where)
		case userCmd[base]:
			add(FlagUsersGroups, "changes users or groups", where)
		case cloudCmd[base]:
			add(FlagCloudCLI, "runs a cloud CLI ("+base+") with the box's credentials", where)
		case aptSrcCmd[base]:
			add(FlagAptSource, "adds or changes an apt source or key", where)
		case base == "systemctl":
			if hasFlag(args, "enable") || hasFlag(args, "reenable") || hasFlag(args, "link") {
				for _, a := range args {
					if written[a] || written[a+".service"] || (strings.HasPrefix(a, "/") && isUnitPath(a)) {
						add(FlagEnableOwnUnit, "enables "+a+", a unit file this batch writes", where)
					}
				}
			}
			for _, a := range args {
				if isSiloUnit(a) {
					add(FlagSiloConfig, "touches silo's own unit "+a, where)
				}
			}
		}
		if base == "usermod" || base == "gpasswd" || base == "adduser" {
			for _, a := range args {
				for _, g := range strings.Split(a, ",") {
					if privilegedGroups[g] {
						add(FlagSudoersPAM, "adds a user to the privileged group "+g, where)
					}
				}
			}
		}
	}
	return out
}

// isUnitPath reports whether p is a systemd unit file location.
func isUnitPath(p string) bool {
	for _, d := range []string{"/etc/systemd/system/", "/lib/systemd/system/", "/usr/lib/systemd/system/", "/run/systemd/system/", "/usr/local/lib/systemd/system/"} {
		if strings.HasPrefix(p, d) && !strings.Contains(strings.TrimPrefix(p, d), "/") {
			return true
		}
	}
	return false
}

func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f || (len(f) == 2 && f[0] == '-' && len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], rune(f[1]))) {
			return true
		}
	}
	return false
}

func anyMatch(args []string, re *regexp.Regexp) bool {
	for _, a := range args {
		if re.MatchString(a) {
			return true
		}
	}
	return false
}

// fields splits shell-ish text on whitespace, quotes and separators, for
// spotting paths inside scripts and -c strings.
func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`"'`+"`"+`;|&<>()=`, r)
	})
}

// unusualChars names the kinds of characters in s a human could misread:
// control characters, bidi controls, other non-ASCII. "" if none.
func unusualChars(s string) string {
	var kinds []string
	seen := map[string]bool{}
	note := func(k string) {
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	for _, r := range s {
		switch {
		case isBidi(r):
			note("bidi-control")
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			note("control")
		case r > 0x7e:
			note("non-ASCII")
		}
	}
	return strings.Join(kinds, ", ")
}

func isBidi(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c
}

// Escape renders s with every control, bidi and non-ASCII character as an
// escape (\n, ‮, …), so quoting and look-alikes cannot hide anything.
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0x7e:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
