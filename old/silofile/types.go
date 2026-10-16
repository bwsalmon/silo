// Package silofile is the Silofile: its Go types, strict YAML decoding,
// silo folder resolution, validation and JSON Schema (design §2).
//
// Every field of design §2.3 has a type here, including the ones marked
// *later*. Those decode, and Validate refuses any value v2.0 does not
// ship with an error naming the stage that adds it, so that later stages
// add values rather than new shapes.
package silofile

// Silofile is one decoded Silofile. Keys are snake_case, and the yaml
// tag of each field is its key. A doc tag is the field's description in
// the JSON Schema; a schema tag of "required" makes the key required.
type Silofile struct {
	Silo        string `yaml:"silo" schema:"required" doc:"Schema version. Must be the first key, and must be v2."`
	Name        string `yaml:"name" schema:"required" doc:"[a-z0-9-]; used in session ids, audit and service-account descriptions."`
	Description string `yaml:"description" doc:"What this silo is for."`

	Agent             Agent             `yaml:"agent" schema:"required" doc:"The agent CLI and how it runs (design §6)."`
	Location          Location          `yaml:"location" doc:"Where the agent CLI runs (design §5). Default: local."`
	RemotePermissions RemotePermissions `yaml:"remote_permissions" doc:"What the CLI's own built-in tools may do on the agent host: none (default), all, or a list of the CLI's own tool names (design §6.1)."`

	Sandboxes   []Sandbox    `yaml:"sandboxes" doc:"Named places where commands run on the agent's behalf (design §7)."`
	MCP         []MCPServer  `yaml:"mcp" doc:"MCP servers routed through the gateway (design §13)."`
	Credentials []Credential `yaml:"credentials" doc:"Secrets, where they come from and where they go (design §8)."`
	Projects    []Project    `yaml:"projects" doc:"GCP projects the agent acts in (design §11)."`
	Git         []GitRepo    `yaml:"git" doc:"Repositories reached through the git proxy (design §9)."`
	Local       *Local       `yaml:"local" doc:"The local executor: scripts on the invoking machine (design §12)."`

	BuiltinTools []string `yaml:"builtin_tools" doc:"Built-in silos only (silo create). Refused in a user Silofile."`

	// Folder is the absolute path of the silo folder and File that of the
	// Silofile, when the Silofile was read by Load. Both are empty after
	// Parse.
	Folder string `yaml:"-"`
	File   string `yaml:"-"`

	// pos records where each YAML path was written, for error messages.
	pos map[string]Pos
}

// Agent is the agent: section.
type Agent struct {
	CLI          string     `yaml:"cli" schema:"required" doc:"claude | codex | agy | custom"`
	Binary       string     `yaml:"binary" doc:"Path of the CLI on the agent host; otherwise found on PATH there."`
	Model        string     `yaml:"model" doc:"The CLI's default otherwise."`
	Effort       string     `yaml:"effort" doc:"Passed where the CLI has a flag for it."`
	Auth         Auth       `yaml:"auth" doc:"ambient (the CLI's own login, the default) or {credential: <name>}."`
	Mode         string     `yaml:"mode" doc:"interactive (default) | headless | attached (later)"`
	Prompt       string     `yaml:"prompt" doc:"Inline text, or a file in the silo folder (a single line with no whitespace is read as a path). Required for headless."`
	SystemPrompt string     `yaml:"system_prompt" doc:"Appended to the CLI's own; inline text or a file, as prompt."`
	Workdir      string     `yaml:"workdir" doc:"On the agent host. Default: a private empty directory."`
	Limits       *Limits    `yaml:"limits"`
	Custom       *CustomCLI `yaml:"custom" doc:"Only for cli: custom (cli-mapping.md §2.4)."`
}

// Limits bounds a session.
type Limits struct {
	Deadline *Duration `yaml:"deadline" doc:"Wall clock, after which the session is ended."`
	MaxTurns int       `yaml:"max_turns" doc:"Where the CLI supports it."`
}

// CustomCLI declares a custom CLI's own mapping (cli-mapping.md §2.4).
type CustomCLI struct {
	Argv        CustomArgv        `yaml:"argv"`
	MCPConfig   CustomMCPConfig   `yaml:"mcp_config"`
	Permissions CustomPermissions `yaml:"permissions"`
	Events      string            `yaml:"events" doc:"How silo parses headless output: claude-stream-json, or none."`
}

// CustomArgv is a custom CLI's arguments per mode.
type CustomArgv struct {
	Interactive []string `yaml:"interactive"`
	Headless    []string `yaml:"headless"`
	Attached    []string `yaml:"attached" doc:"Later."`
}

// CustomMCPConfig says how a custom CLI is given its MCP config.
type CustomMCPConfig struct {
	Format string   `yaml:"format" doc:"claude-json | codex-toml | stdio-bridge"`
	Arg    []string `yaml:"arg" doc:"Arguments naming the file, with {file} substituted."`
	Env    string   `yaml:"env" doc:"Or an environment variable naming the file."`
}

// CustomPermissions are a custom CLI's arguments per remote_permissions
// setting. A setting with no entry is refused.
type CustomPermissions struct {
	None *[]string `yaml:"none"`
	All  *[]string `yaml:"all"`
}

// Location is the location: section.
type Location struct {
	Kind string `yaml:"kind" doc:"local (default) | ssh | k8s | vm (later) | ax (later) | capsule (later)"`
	SSH  *SSH   `yaml:"ssh" doc:"Only for kind: ssh."`
	K8s  *K8s   `yaml:"k8s" doc:"Only for kind: k8s."`
}

// K8s is location.k8s (design §5.4, journey 8).
type K8s struct {
	Context     string        `yaml:"context" doc:"A context in the kubeconfig; kubectl's current context when empty."`
	Namespace   string        `yaml:"namespace" doc:"The namespace the session's pod and Secret go in; the context's namespace when empty."`
	Image       string        `yaml:"image" schema:"required" doc:"The image the agent CLI runs in; it needs /bin/sh and the CLI on its PATH."`
	TunnelImage string        `yaml:"tunnel_image" doc:"The gateway tunnel sidecar's image (design §5.4); silo's published one when empty."`
	Resources   *K8sResources `yaml:"resources" doc:"The agent container's requests and limits."`
	Identity    *K8sIdentity  `yaml:"identity" doc:"Later: workload identity (design §5.4)."`
}

// K8sResources is location.k8s.resources.
type K8sResources struct {
	CPU    string `yaml:"cpu" doc:"A Kubernetes CPU quantity, such as 2 or 500m."`
	Memory *Size  `yaml:"memory" doc:"A size, such as 4Gi."`
}

// K8sIdentity is location.k8s.identity (later).
type K8sIdentity struct {
	GCPAccountFor string `yaml:"gcp_account_for" doc:"The projects[] entry whose per-user account the pod's service account is bound to."`
}

// SSH is location.ssh (design §5.3).
type SSH struct {
	Host             string `yaml:"host" schema:"required" doc:"A Host alias in ~/.ssh/config, or a hostname."`
	User             string `yaml:"user"`
	Port             int    `yaml:"port"`
	Identity         string `yaml:"identity" doc:"An identity file; may be absolute or ~/..."`
	GatewayTransport string `yaml:"gateway_transport" doc:"tcp (default) | unix (later)"`
}

// Sandbox is one sandboxes[] entry (design §7).
type Sandbox struct {
	Name    string            `yaml:"name" schema:"required"`
	Kind    string            `yaml:"kind" schema:"required" doc:"container | gvisor | granule | folder (built-in silos only)"`
	Image   string            `yaml:"image" doc:"An OCI image, or for granule a granule VM image."`
	Build   string            `yaml:"build" doc:"A Dockerfile in the silo folder, built into the image."`
	Workdir string            `yaml:"workdir"`
	CPUs    float64           `yaml:"cpus"`
	Memory  *Size             `yaml:"memory" doc:"A size, such as 4Gi."`
	Disk    *Size             `yaml:"disk" doc:"A size, such as 20Gi."`
	Network Network           `yaml:"network" doc:"none (default) | open | {allow: [host, ...]} (later)"`
	Mounts  []Mount           `yaml:"mounts" doc:"Read-only unless rw: true."`
	Repos   []string          `yaml:"repos" doc:"git[] entries checked out through the git proxy: one at workdir, several each at workdir/<name>. Needs an absolute workdir and git in the image."`
	Env     map[string]string `yaml:"env"`

	Backend      Backend       `yaml:"backend" doc:"local (default) | {k8s: {context, namespace}}: a granule sandbox as a pod in a cluster (journey 9)."`
	Lifetime     string        `yaml:"lifetime" doc:"session (default) | persistent (later)"`
	PackageCache *PackageCache `yaml:"package_cache" doc:"Later (grain.md §2.3)."`
	ModelAccess  []string      `yaml:"model_access" doc:"Later: the model proxy (grain.md §2.5)."`

	// kind: granule, in place of image (journey 10): the granule
	// counterpart of build.
	Granulefile string            `yaml:"granulefile" doc:"Granule only, instead of image: a Granulefile in the silo folder whose target stage builds a granule VM image, built with its directory as the context."`
	Target      string            `yaml:"target" doc:"Granule only: the Granulefile stage to build, by name or index; the last stage when empty."`
	Args        map[string]string `yaml:"args" doc:"Granule only: the Granulefile's build arguments (--build-arg)."`
	PushTo      string            `yaml:"push_to" doc:"Granule only, with granulefile: a registry repository the built image is pushed to, for a remote backend (stage 11b); the local backend pushes nothing."`

	// kind: folder (built-in silos only).
	Path string `yaml:"path" doc:"Folder sandboxes only (built-in silos)."`
	RW   bool   `yaml:"rw" doc:"Folder sandboxes only (built-in silos)."`
}

// Mount is one sandboxes[].mounts entry.
type Mount struct {
	From string `yaml:"from" schema:"required" doc:"A path in the silo folder."`
	To   string `yaml:"to" schema:"required" doc:"An absolute path in the sandbox."`
	RW   bool   `yaml:"rw"`
}

// PackageCache is a sandbox's package-cache hook (grain.md §2.3).
type PackageCache struct {
	URL string `yaml:"url"`
	CA  string `yaml:"ca"`
}

// MCPServer is one mcp[] entry (design §13.1).
type MCPServer struct {
	Name       string            `yaml:"name" schema:"required" doc:"The gateway's prefix for the server's tools."`
	Where      string            `yaml:"where" doc:"local (the invoking machine, default) | remote (the agent host)"`
	From       string            `yaml:"from" doc:"<source>:<name>, source one of claude-user, claude-project, mcp-json, codex, agy."`
	Command    []string          `yaml:"command" doc:"argv of a server silo starts, over stdio."`
	URL        string            `yaml:"url" doc:"An already-running server, over Streamable HTTP."`
	Headers    map[string]string `yaml:"headers" doc:"For url: servers."`
	Tools      []string          `yaml:"tools" doc:"Allowlist of the server's tool names; default all."`
	Env        map[string]string `yaml:"env"`
	InheritEnv InheritEnv        `yaml:"inherit_env" doc:"Names passed through from silo's environment, or all."`
	Required   *bool             `yaml:"required" doc:"Default true."`
	Restart    string            `yaml:"restart" doc:"never (default) | on-failure"`
}

// Credential is one credentials[] entry (design §8).
type Credential struct {
	Name string        `yaml:"name" schema:"required"`
	From Source        `yaml:"from" schema:"required" doc:"Exactly one of env, file, store, ambient."`
	To   []Destination `yaml:"to" doc:"Where the credential is delivered; at least one, unless agent.auth names it."`
}

// Source is where a credential comes from (design §8.1). Exactly one
// field is set.
type Source struct {
	Env     string `yaml:"env" doc:"A variable of the invoking environment."`
	File    string `yaml:"file" doc:"A file on the invoking machine; may be absolute or ~/..."`
	Store   string `yaml:"store" doc:"A name in silo's secret store."`
	Ambient string `yaml:"ambient" doc:"gh | git-credential | gcloud (gcloud is silo's own and is never delivered)."`
}

// Destination is one place a credential is delivered (design §8.2):
// {sandbox, env|file}, {mcp, env}, {local, env}, {agent: env, name},
// {proxy: git|model}.
type Destination struct {
	Sandbox string `yaml:"sandbox"`
	MCP     string `yaml:"mcp"`
	Local   string `yaml:"local" doc:"A local-executor script, by name."`
	Agent   string `yaml:"agent" doc:"env: set in the agent CLI's own environment."`
	Proxy   string `yaml:"proxy" doc:"git | model (later)"`
	Env     string `yaml:"env"`
	File    string `yaml:"file"`
	Name    string `yaml:"name" doc:"The variable name, with agent: env."`
}

// Project is one projects[] entry (design §11).
type Project struct {
	Project         string    `yaml:"project" schema:"required"`
	Roles           []string  `yaml:"roles"`
	TTL             *Duration `yaml:"ttl" doc:"Idle time before silo gc may delete the account; 0 deletes it on exit."`
	AllowBasicRoles bool      `yaml:"allow_basic_roles" doc:"roles/owner, roles/editor and roles/viewer need this."`
	DeliverTo       []string  `yaml:"deliver_to" doc:"Sandboxes that get the token."`
	Account         string    `yaml:"account" doc:"Later (grain.md §2.6): a caller-provided identity instead of roles."`
	AttachTo        string    `yaml:"attach_to" doc:"Later (stage 10, journey 5)."`
}

// GitRepo is one git[] entry (design §9).
type GitRepo struct {
	Name       string   `yaml:"name" schema:"required"`
	Repo       string   `yaml:"repo" schema:"required" doc:"host/owner/name, such as github.com/acme/widgets."`
	Access     string   `yaml:"access" doc:"read (default) | push"`
	Push       []string `yaml:"push" doc:"Ref patterns a push may update: refs/heads/... or refs/tags/..., globbing with * and ?, each optionally followed by (force), (delete) or (force, delete)."`
	Force      bool     `yaml:"force" doc:"false (default): non-fast-forward pushes are refused."`
	Credential string   `yaml:"credential" doc:"A credentials[] entry delivered to {proxy: git}."`
}

// Local is the local executor (design §12).
type Local struct {
	Scripts   *Scripts  `yaml:"scripts"`
	Workdir   string    `yaml:"workdir" doc:"A directory in the silo folder; default the folder itself."`
	Timeout   *Duration `yaml:"timeout" doc:"Default 5m."`
	OutputCap *Size     `yaml:"output_cap" doc:"Per stream; default 1MiB."`
}

// Scripts says which scripts the local executor serves.
type Scripts struct {
	Dir  string   `yaml:"dir" doc:"Every executable directly in it; default scripts."`
	List []Script `yaml:"list"`
}

// Script is one local.scripts.list entry.
type Script struct {
	Path        string    `yaml:"path" schema:"required"`
	Description string    `yaml:"description"`
	Approve     bool      `yaml:"approve" doc:"Each call waits for the user's approval."`
	Timeout     *Duration `yaml:"timeout"`
}
