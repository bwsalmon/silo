package silofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// folder writes a silo folder: a Silofile and files, where a file whose
// content starts with #! is made executable.
func folder(t *testing.T, silofile string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["Silofile"] = silofile
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(content, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func validate(t *testing.T, dir string, opts Options) Result {
	t.Helper()
	f, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return f.Validate(opts)
}

const script = "#!/bin/sh\necho hi\n"

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string // appended to minimal unless it starts with "silo:"
		files   map[string]string
		opts    Options
		wantErr string // substring of one error; empty means valid
		later   string // the error is a later one, naming this
	}{
		{name: "minimal", yaml: ""},
		{name: "bad silo name", yaml: "silo: v2\nname: Big_Name\nagent: {cli: claude}\n", wantErr: `"Big_Name" is not a silo name`},
		{name: "unknown cli", yaml: "silo: v2\nname: t\nagent: {cli: gpt}\n", wantErr: `"gpt" is not one of claude, codex, agy, custom`},

		// Names and cross-references.
		{name: "duplicate sandbox", yaml: "sandboxes:\n  - {name: a, kind: container, image: x}\n  - {name: a, kind: gvisor, image: y}\n", wantErr: `"a" is already the name of another sandboxes entry`},
		{name: "duplicate mcp", yaml: "mcp:\n  - {name: a, command: [x]}\n  - {name: a, command: [y]}\n", wantErr: `"a" is already the name of another mcp entry`},
		{name: "duplicate credential", yaml: "credentials:\n  - {name: a, from: {env: A}, to: [{proxy: git}]}\n  - {name: a, from: {env: B}, to: [{proxy: git}]}\n", wantErr: `already the name of another credentials entry`},
		{name: "duplicate git", yaml: "git:\n  - {name: a, repo: github.com/a/b}\n  - {name: a, repo: github.com/a/c}\n", wantErr: `already the name of another git entry`},
		{name: "duplicate project", yaml: "projects:\n  - {project: p, roles: [roles/x]}\n  - {project: p, roles: [roles/y]}\n", wantErr: `already another entry's project`},
		{name: "to unknown sandbox", yaml: "credentials:\n  - {name: c, from: {env: A}, to: [{sandbox: nope, env: X}]}\n", wantErr: `no sandbox is named "nope"`},
		{name: "to unknown mcp", yaml: "credentials:\n  - {name: c, from: {env: A}, to: [{mcp: nope, env: X}]}\n", wantErr: `no MCP server is named "nope"`},
		{name: "to url mcp", yaml: "mcp:\n  - {name: m, url: http://x}\ncredentials:\n  - {name: c, from: {env: A}, to: [{mcp: m, env: X}]}\n", wantErr: `url server, which silo does not start`},
		{name: "to unknown script", yaml: "local: {}\ncredentials:\n  - {name: c, from: {env: A}, to: [{local: nope, env: X}]}\n", files: map[string]string{"scripts/run": script}, wantErr: `no local script is named "nope"`},
		{name: "unknown repo", yaml: "sandboxes:\n  - {name: a, kind: container, image: x, repos: [nope]}\n", wantErr: `no git entry is named "nope"`},
		{name: "unknown git credential", yaml: "git:\n  - {name: a, repo: github.com/a/b, credential: nope}\n", wantErr: `no credential is named "nope"`},
		{name: "git credential not for git", yaml: "git:\n  - {name: a, repo: github.com/a/b, credential: c}\nsandboxes:\n  - {name: s, kind: container, image: x}\ncredentials:\n  - {name: c, from: {env: A}, to: [{sandbox: s, env: X}]}\n", wantErr: `not delivered to {proxy: git}`},
		{name: "unknown deliver_to", yaml: "projects:\n  - {project: p, roles: [roles/x], deliver_to: [nope]}\n", wantErr: `no sandbox is named "nope"`},
		{name: "unknown auth credential", yaml: "  auth: {credential: nope}\n", wantErr: `no credential is named "nope"`},
		{name: "cross-references resolve", yaml: `  auth: {credential: key}
sandboxes:
  - {name: s, kind: container, image: x, workdir: /w, repos: [r]}
git:
  - {name: r, repo: github.com/a/b, access: push, push: [refs/heads/x, refs/heads/y (delete)], credential: gh}
credentials:
  - {name: key, from: {env: K}}
  - {name: gh, from: {ambient: gh}, to: [{proxy: git}, {sandbox: s, file: /run/gh}]}
projects:
  - {project: p, roles: [roles/logging.viewer], deliver_to: [s], ttl: 0}
`},

		{name: "repos need workdir", yaml: "sandboxes:\n  - {name: s, kind: container, image: x, repos: [r]}\ngit:\n  - {name: r, repo: github.com/a/b}\n", wantErr: "repos are checked out into workdir"},
		{name: "bad push pattern", yaml: "git:\n  - {name: r, repo: github.com/a/b, access: push, push: [refs/heads/*]}\n", wantErr: "would match every ref"},
		{name: "bad push flag", yaml: "git:\n  - {name: r, repo: github.com/a/b, access: push, push: [refs/heads/x (merge)]}\n", wantErr: "is not a flag"},
		{name: "git credential ambiguous", yaml: "git:\n  - {name: r, repo: github.com/a/b}\ncredentials:\n  - {name: a, from: {env: A}, to: [{proxy: git}]}\n  - {name: b, from: {env: B}, to: [{proxy: git}]}\n", wantErr: "name the one this repository uses"},
		{name: "git credential implied", yaml: "git:\n  - {name: r, repo: github.com/a/b}\ncredentials:\n  - {name: a, from: {env: A}, to: [{proxy: git}]}\n"},

		// Files and the folder.
		{name: "prompt file missing", yaml: "  prompt: prompts/task.md\n", wantErr: `"prompts/task.md" does not exist in the silo folder`},
		{name: "prompt file present", yaml: "  prompt: prompts/task.md\n", files: map[string]string{"prompts/task.md": "hi"}},
		{name: "inline prompt", yaml: "  mode: headless\n  prompt: Fix the bug.\n"},
		{name: "headless without prompt", yaml: "  mode: headless\n", wantErr: "mode headless needs a prompt"},
		{name: "dotdot out", yaml: "  system_prompt: ../outside.md\n", wantErr: `"../outside.md" leaves the silo folder`},
		{name: "absolute prompt", yaml: "  system_prompt: /etc/passwd\n", wantErr: `"/etc/passwd" is outside the silo folder`},
		{name: "build missing", yaml: "sandboxes:\n  - {name: a, kind: container, build: sandboxes/tools.Dockerfile}\n", wantErr: `does not exist`},
		{name: "mount out", yaml: "sandboxes:\n  - {name: a, kind: container, image: x, mounts: [{from: .., to: /x}]}\n", wantErr: `".." leaves the silo folder`},
		{name: "mount to relative", yaml: "sandboxes:\n  - {name: a, kind: container, image: x, mounts: [{from: ., to: x}]}\n", wantErr: `not an absolute path in the sandbox`},
		{name: "ssh identity absolute", yaml: "location: {kind: ssh, ssh: {host: h, identity: ~/.ssh/id_ed25519}}\nremote_permissions: all\n"},
		{name: "ssh identity relative missing", yaml: "location: {kind: ssh, ssh: {host: h, identity: keys/id}}\n", wantErr: `"keys/id" does not exist`},
		{name: "mcp-json default missing", yaml: "mcp:\n  - {name: d, from: mcp-json:docs}\n", wantErr: `neither mcp/servers.json nor .mcp.json`},
		{name: "mcp-json default present", yaml: "mcp:\n  - {name: d, from: mcp-json:docs}\n", files: map[string]string{".mcp.json": "{}"}},
		{name: "mcp-json absolute", yaml: "mcp:\n  - {name: d, from: mcp-json:docs@/etc/mcp.json}\n"},
		{name: "mcp-json relative missing", yaml: "mcp:\n  - {name: d, from: mcp-json:docs@conf/x.json}\n", wantErr: `"conf/x.json" does not exist`},
		{name: "claude-project needs dir", yaml: "mcp:\n  - {name: d, from: claude-project:docs}\n", wantErr: `needs @<dir>`},
		{name: "unknown source", yaml: "mcp:\n  - {name: d, from: vscode:docs}\n", wantErr: `is not <source>:<name>`},
		{name: "mcp two kinds", yaml: "mcp:\n  - {name: d, command: [x], url: http://y}\n", wantErr: `exactly one of from, command and url`},

		// The local executor.
		{name: "default scripts dir", yaml: "local: {}\n", files: map[string]string{"scripts/run": script, "scripts/.hidden": script, "scripts/data.txt": "x"}},
		{name: "no scripts", yaml: "local: {}\n", wantErr: "the local executor has no scripts"},
		{name: "listed not executable", yaml: "local: {scripts: {list: [{path: tools/run}]}}\n", files: map[string]string{"tools/run": "echo"}, wantErr: `"run" is not executable`},
		{name: "explicit dir missing", yaml: "local: {scripts: {dir: bin}}\n", wantErr: `"bin" does not exist`},
		{name: "two scripts one tool", yaml: "local: {}\n", files: map[string]string{"scripts/a-b": script, "scripts/a_b": script}, wantErr: `both become the tool local_a_b`},
		{name: "script credential", yaml: "local: {}\ncredentials:\n  - {name: c, from: {store: x}, to: [{local: run, env: T}]}\n", files: map[string]string{"scripts/run": script}},

		// remote_permissions per CLI (cli-mapping.md §2).
		{name: "claude subset", yaml: "remote_permissions: [Read, Grep]\n"},
		{name: "empty subset", yaml: "remote_permissions: []\n", wantErr: "an empty list"},
		{name: "codex none", yaml: "silo: v2\nname: t\nagent: {cli: codex}\nremote_permissions: none\n"},
		{name: "codex all", yaml: "silo: v2\nname: t\nagent: {cli: codex}\nremote_permissions: all\n"},
		{name: "codex subset", yaml: "silo: v2\nname: t\nagent: {cli: codex}\nremote_permissions: [Read]\n", wantErr: "codex cannot express a subset"},
		{name: "agy default none", yaml: "silo: v2\nname: t\nagent: {cli: agy}\n", wantErr: "agy cannot express none"},
		{name: "agy subset", yaml: "silo: v2\nname: t\nagent: {cli: agy}\nremote_permissions: [Read]\n", wantErr: "agy cannot express a subset"},
		{name: "agy all", yaml: "silo: v2\nname: t\nagent: {cli: agy}\nremote_permissions: all\n"},
		{name: "custom none undeclared", yaml: "silo: v2\nname: t\nagent: {cli: custom, binary: /x, custom: {mcp_config: {format: claude-json, arg: [--mcp, '{file}']}, permissions: {all: []}}}\n", wantErr: "none needs agent.custom.permissions.none"},
		{name: "custom none declared", yaml: "silo: v2\nname: t\nagent: {cli: custom, binary: /x, custom: {mcp_config: {format: claude-json, env: X}, permissions: {none: [--no-tools]}}}\n"},
		{name: "custom all undeclared", yaml: "silo: v2\nname: t\nagent: {cli: custom, binary: /x, custom: {mcp_config: {format: claude-json, env: X}, permissions: {none: []}}}\nremote_permissions: all\n", wantErr: "all needs agent.custom.permissions.all"},
		{name: "custom subset", yaml: "silo: v2\nname: t\nagent: {cli: custom, binary: /x, custom: {mcp_config: {format: claude-json, env: X}}}\nremote_permissions: [Read]\n", wantErr: "a custom CLI cannot express a subset"},
		{name: "custom without mapping", yaml: "silo: v2\nname: t\nagent: {cli: custom, binary: /x}\n", wantErr: "needs a custom: mapping"},
		{name: "custom on claude", yaml: "  custom: {events: none}\n", wantErr: "custom is only for cli: custom"},

		// Credentials and their destinations (design §8).
		{name: "no destination", yaml: "credentials:\n  - {name: c, from: {env: A}, to: []}\n", wantErr: `credential "c" is delivered nowhere`},
		{name: "auth is a destination", yaml: "  auth: {credential: c}\ncredentials:\n  - {name: c, from: {env: A}, to: []}\n"},
		{name: "gcloud refused", yaml: "credentials:\n  - {name: c, from: {ambient: gcloud}, to: [{proxy: git}]}\n", wantErr: "{ambient: gcloud} is silo's own"},
		{name: "two sources", yaml: "credentials:\n  - {name: c, from: {env: A, store: b}, to: [{proxy: git}]}\n", wantErr: "exactly one of env, file, store and ambient"},
		{name: "unknown ambient", yaml: "credentials:\n  - {name: c, from: {ambient: aws}, to: [{proxy: git}]}\n", wantErr: `"aws" is not one of gh, git-credential, gcloud`},
		{name: "mixed destination", yaml: "sandboxes:\n  - {name: s, kind: container, image: x}\ncredentials:\n  - {name: c, from: {env: A}, to: [{sandbox: s, env: X, name: Y}]}\n", wantErr: "name does not go with sandbox"},
		{name: "empty destination", yaml: "credentials:\n  - {name: c, from: {env: A}, to: [{env: X}]}\n", wantErr: "name a destination"},
		{name: "credential file outside", yaml: "credentials:\n  - {name: c, from: {file: ~/.config/token}, to: [{proxy: git}]}\n"},

		// location.k8s (design §5.4).
		{name: "k8s", yaml: "location: {kind: k8s, k8s: {context: kind-a, namespace: silo, image: x, resources: {cpu: 500m, memory: 1Gi}}}\n"},
		{name: "k8s missing", yaml: "location: {kind: k8s}\n", wantErr: "kind k8s needs k8s"},
		{name: "k8s bad namespace", yaml: "location: {kind: k8s, k8s: {image: x, namespace: Silo_1}}\n", wantErr: "is not a namespace name"},
		{name: "k8s bad cpu", yaml: "location: {kind: k8s, k8s: {image: x, resources: {cpu: two}}}\n", wantErr: "is not a CPU quantity"},
		{name: "k8s on local", yaml: "location: {k8s: {image: x}}\n", wantErr: "k8s is only for kind: k8s"},

		// Projects.
		{name: "basic role", yaml: "projects:\n  - {project: p, roles: [roles/viewer]}\n", wantErr: "roles/viewer is a basic role"},
		{name: "basic role allowed", yaml: "projects:\n  - {project: p, roles: [roles/viewer], allow_basic_roles: true}\n"},

		// Built-in only (design §2.3, call 10).
		{name: "builtin_tools", yaml: "builtin_tools: [silo_schema]\n", wantErr: "only silo's built-in silos may use builtin_tools"},
		{name: "folder kind", yaml: "sandboxes:\n  - {name: w, kind: folder, path: /tmp, rw: true}\n", wantErr: "kind folder is for silo's built-in silos only"},
		{name: "builtin allowed", yaml: "builtin_tools: [silo_schema]\nsandboxes:\n  - {name: w, kind: folder, path: /tmp, rw: true}\n", opts: Options{Builtin: true}},

		// Granule (stage 8a).
		{name: "granule", yaml: "sandboxes:\n  - {name: g, kind: granule, image: x, cpus: 2, memory: 4Gi, disk: 20Gi}\n"},
		{name: "granule no image", yaml: "sandboxes:\n  - {name: g, kind: granule}\n", wantErr: "kind granule needs image"},
		{name: "granule build", yaml: "sandboxes:\n  - {name: g, kind: granule, build: Dockerfile}\n", wantErr: "booted from a granule VM image"},
		{name: "granule mounts", yaml: "sandboxes:\n  - {name: g, kind: granule, image: x, mounts: [{from: ., to: /src}]}\n", wantErr: "kind granule takes no mounts"},

		// Later values decode, and are refused naming what adds them.
		{name: "granulefile", yaml: "sandboxes:\n  - {name: g, kind: granule, granulefile: dev.Granulefile}\n", later: "stage 8b"},
		{name: "location vm", yaml: "location: {kind: vm}\n", later: "stage 10"},
		{name: "k8s identity", yaml: "location: {kind: k8s, k8s: {image: x, identity: {gcp_account_for: p}}}\n", later: "design §5.4"},
		{name: "location ax", yaml: "location: {kind: ax}\n", later: "stage 12"},
		{name: "attached", yaml: "  mode: attached\n", later: "design §6.2"},
		{name: "unix transport", yaml: "location: {kind: ssh, ssh: {host: h, gateway_transport: unix}}\n", later: "design §5.3"},
		{name: "network allow", yaml: "sandboxes:\n  - {name: s, kind: container, image: x, network: {allow: [a.example]}}\n", later: "grain.md §2.3"},
		{name: "k8s backend", yaml: "sandboxes:\n  - {name: s, kind: granule, image: x, backend: {k8s: {context: c, namespace: n}}}\n"},
		{name: "k8s backend container", yaml: "sandboxes:\n  - {name: s, kind: container, image: x, backend: {k8s: {context: c}}}\n", wantErr: "backend k8s runs kind granule sandboxes only"},
		{name: "k8s backend repos", yaml: "git:\n  - {name: r, repo: github.com/a/b}\nsandboxes:\n  - {name: s, kind: granule, image: x, workdir: /w, repos: [r], backend: {k8s: {}}}\n", later: "design §9.3"},
		{name: "persistent", yaml: "sandboxes:\n  - {name: s, kind: container, image: x, lifetime: persistent}\n", later: "grain.md §2.3"},
		{name: "model proxy", yaml: "credentials:\n  - {name: c, from: {env: A}, to: [{proxy: model}]}\n", later: "grain.md §2.5"},
		{name: "account", yaml: "projects:\n  - {project: p, account: a@p.iam.gserviceaccount.com}\n", later: "grain.md §2.6"},
		{name: "attach_to", yaml: "projects:\n  - {project: p, roles: [roles/x], attach_to: vm}\n", later: "stage 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y := tc.yaml
			if !strings.HasPrefix(y, "silo:") {
				y = minimal + y
			}
			if tc.files == nil {
				tc.files = map[string]string{}
			}
			res := validate(t, folder(t, y, tc.files), tc.opts)
			switch {
			case tc.later != "":
				if len(res.Errors) != 1 || res.Errors[0].Later != tc.later || !strings.Contains(res.Errors[0].Error(), tc.later) {
					t.Fatalf("errors = %v, want one naming %s", res.Errors, tc.later)
				}
			case tc.wantErr == "":
				if err := res.Err(); err != nil {
					t.Fatalf("Validate: %v", err)
				}
			default:
				if !strings.Contains(res.Errors.Error(), tc.wantErr) {
					t.Fatalf("errors = %v, want one containing %q", res.Errors, tc.wantErr)
				}
				for _, e := range res.Errors {
					if e.Line == 0 || !strings.HasSuffix(e.File, "Silofile") {
						t.Errorf("error %q has no file or line", e)
					}
				}
			}
		})
	}
}

func TestValidateSymlinkOut(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "tool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := folder(t, minimal+"  prompt: prompts/task.md\nlocal: {}\n", map[string]string{})
	for link, target := range map[string]string{"prompts": outside, "scripts/tool": filepath.Join(outside, "tool"), "scripts/ok": ""} {
		p := filepath.Join(dir, link)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if target == "" {
			os.WriteFile(p, []byte(script), 0o755)
			continue
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	res := validate(t, dir, Options{})
	got := res.Errors.Error()
	for _, want := range []string{`agent.prompt: "prompts/task.md" leaves the silo folder`, `local: "scripts/tool" leaves the silo folder`} {
		if !strings.Contains(got, want) {
			t.Errorf("errors = %v, want %q", got, want)
		}
	}
}

func TestValidateWarnsAgentDelivery(t *testing.T) {
	res := validate(t, folder(t, minimal+"credentials:\n  - {name: c, from: {env: A}, to: [{agent: env, name: TOKEN}]}\n", map[string]string{}), Options{})
	if res.Err() != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Msg, "the agent can read it") {
		t.Fatalf("result = %+v", res)
	}
}

func TestValidateScriptHook(t *testing.T) {
	dir := folder(t, minimal+"local: {scripts: {list: [{path: scripts/Deploy.sh, description: Deploy.}]}}\n", map[string]string{"scripts/Deploy.sh": script, "scripts/logs": script})
	var seen []LocalScript
	res := validate(t, dir, Options{CheckScripts: func(f *Silofile, scripts []LocalScript) (Errors, Errors) {
		seen = scripts
		return nil, Errors{{Msg: "eval"}}
	}})
	if res.Err() != nil || len(res.Warnings) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(seen) != 2 || seen[0].Tool != "local_deploy_sh" || seen[0].Entry == nil || seen[1].Name != "logs" || seen[1].Entry != nil {
		t.Fatalf("scripts = %+v", seen)
	}
}

func TestLoadBareFile(t *testing.T) {
	dir := folder(t, "", map[string]string{"ops.Silofile": minimal + "  prompt: prompts/x.md\n", "prompts/x.md": "hi"})
	f, err := Load(filepath.Join(dir, "ops.Silofile"))
	if err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(dir); f.Folder != dir && f.Folder != real {
		t.Errorf("Folder = %s, want %s", f.Folder, dir)
	}
	if err := f.Validate(Options{}).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "has no Silofile") {
		t.Errorf("Load(empty dir) = %v", err)
	}
}
