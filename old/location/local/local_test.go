package local

import (
	"strings"
	"testing"
)

func TestScrubKeepsOnlyTheAllowlist(t *testing.T) {
	got := Scrub([]string{
		"PATH=/bin", "HOME=/h", "USER=u", "LOGNAME=u", "SHELL=/bin/sh", "TERM=xterm", "LANG=C",
		"LC_ALL=C", "TMPDIR=/tmp", "XDG_CONFIG_HOME=/c",
		"AWS_SECRET_ACCESS_KEY=x", "GITHUB_TOKEN=y", "ANTHROPIC_API_KEY=z", "SSH_AUTH_SOCK=/s", "PATHX=1",
	})
	want := "PATH=/bin HOME=/h USER=u LOGNAME=u SHELL=/bin/sh TERM=xterm LANG=C LC_ALL=C TMPDIR=/tmp XDG_CONFIG_HOME=/c"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %v\nwant %s", got, want)
	}
}
