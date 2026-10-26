#!/usr/bin/env bash
# install.sh: install silo v3 on a Debian 12 (bookworm) or 13 (trixie) box.
#
# Run as root. Idempotent: running it again upgrades the binary and the
# pinned CLIs, rewrites silo's units and keeps state, config, users, homes
# and the UI login token.
#
#   sudo ./install.sh                      # build silo from this checkout (needs Go >= 1.24)
#   sudo ./install.sh --release [VERSION]  # download a release binary instead
#   sudo ./install.sh --binary ./silo      # install a binary you already have
#
# Other options:
#   --source DIR      the v3 module to build from (default: the checkout this script is in)
#   --no-cli          skip Node.js and the coding-agent CLIs
#   --runtime NAME    CLI preset the two standing agents run, for a new
#                     /etc/silo/gateway.toml (default claude; gemini, codex, or
#                     any /etc/silo/clis.d preset)
#   --listen ADDR     the UI's listen address (default 127.0.0.1:7070)
#   --rotate-token    replace the UI login token
#   --no-start        install everything but do not enable or start the units
#
# Versions are pinned in versions.env beside this script.
#
# What it sets up (design §11, Appendix B):
#   /usr/local/bin/silo                      the silo binary
#   /opt/silo/cli/                           pinned CLIs and ACP adapters (root-owned)
#   /etc/silo/{gateway.toml,clis.d,root.d,secrets}
#   /usr/share/silo/root.md                  shipped root agent instructions
#   /var/lib/silo/{gateway,sessions,ui,home/<user>}
#   /var/log/silo/                           audit log
#   /run/silo/                               sockets (recreated at boot by tmpfiles.d)
#   users silo (bridge/UI), silo-root (the root agent), silo-user (the default user agent)
#   silo-gateway.service  silo-ui.service  silo-root.service  silo-user.service
set -euo pipefail
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

PREFIX_BIN=/usr/local/bin
CLI_DIR=/opt/silo/cli
ETC=/etc/silo
STATE=/var/lib/silo
LOGDIR=/var/log/silo
RUNDIR=/run/silo
SHARE=/usr/share/silo
UNITDIR=/etc/systemd/system
UI_TOKEN_HASH="$STATE/ui/token.sha256"
UI_TOKEN_RAW="$ETC/ui-token"

BRIDGE_USER=silo
GW_GROUP=silo-gw
ROOT_AGENT=root
ROOT_USER=silo-root
USER_AGENT=main
USER_USER=silo-user

mode=""
source_dir=""
binary=""
release=""
with_cli=1
runtime=claude
listen=127.0.0.1:7070
rotate=0
start=1

log() { printf '==> %s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

usage() { sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
	case "$1" in
	--source) mode=source; source_dir="${2:?--source needs a directory}"; shift ;;
	--binary) mode=binary; binary="${2:?--binary needs a path}"; shift ;;
	--release)
		mode=release
		if [ $# -gt 1 ] && [ "${2#-}" = "$2" ]; then release="$2"; shift; fi ;;
	--no-cli) with_cli=0 ;;
	--runtime) runtime="${2:?--runtime needs a name}"; shift ;;
	--listen) listen="${2:?--listen needs an address}"; shift ;;
	--rotate-token) rotate=1 ;;
	--no-start) start=0 ;;
	-h|--help) usage; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
	shift
done

[ "$(id -u)" -eq 0 ] || die "run as root (sudo $0)"
[ -r "$SCRIPT_DIR/versions.env" ] || die "missing $SCRIPT_DIR/versions.env"
# shellcheck source=versions.env
. "$SCRIPT_DIR/versions.env"
release="${release:-$SILO_VERSION}"
release="${release#v}"
[[ "$runtime" =~ ^[a-z][a-z0-9_.-]{0,31}$ ]] || die "--runtime $runtime: not a preset name"
[[ "$listen" =~ ^[^[:space:]\"\']+:[0-9]+$ ]] || die "--listen $listen: want HOST:PORT"

# ---- requirements --------------------------------------------------------

check_system() {
	. /etc/os-release
	case "${ID:-}:${VERSION_ID:-}" in
	debian:12|debian:13) ;;
	*) log "warning: tested on Debian 12 and 13 only; this is ${PRETTY_NAME:-unknown}" ;;
	esac
	[ -d /run/systemd/system ] || die "systemd is not running as init"
	local sdver
	sdver="$(systemctl --version | awk 'NR==1 {print $2}')"
	[ "${sdver%%.*}" -ge 252 ] || die "systemd $sdver is older than 252"
	[ -f /sys/fs/cgroup/cgroup.controllers ] || die "the unified cgroup v2 hierarchy is not mounted"
}

apt_install() {
	local missing=()
	for p in "$@"; do
		dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'install ok installed' || missing+=("$p")
	done
	[ ${#missing[@]} -eq 0 ] && return 0
	log "apt-get install ${missing[*]}"
	DEBIAN_FRONTEND=noninteractive apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "${missing[@]}"
}

# ---- the silo binary -----------------------------------------------------

find_go() {
	if [ -n "${GO:-}" ]; then echo "$GO"; return; fi
	if command -v go >/dev/null 2>&1; then command -v go; return; fi
	# sudo's secure_path drops the usual tarball location.
	for g in /usr/local/go/bin/go /usr/lib/go/bin/go; do
		[ -x "$g" ] && { echo "$g"; return; }
	done
}

install_silo() {
	local tmp
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN
	if [ -z "$mode" ]; then
		if [ -f "$SCRIPT_DIR/../go.mod" ] && [ -n "$(find_go)" ]; then
			mode=source
		else
			die "no Go toolchain to build from source: pass --release [VERSION] or --binary PATH"
		fi
	fi
	case "$mode" in
	source)
		source_dir="${source_dir:-$SCRIPT_DIR/..}"
		[ -f "$source_dir/go.mod" ] || die "$source_dir has no go.mod"
		local go
		go="$(find_go)"
		[ -n "$go" ] || die "--source needs Go >= 1.24 (set GO=/path/to/go)"
		log "building silo from $source_dir with $("$go" version)"
		(cd "$source_dir" && CGO_ENABLED=0 "$go" build -trimpath -o "$tmp/silo" ./cmd/silo)
		;;
	binary)
		[ -f "$binary" ] || die "--binary $binary: no such file"
		cp "$binary" "$tmp/silo"
		;;
	release)
		local arch base url
		case "$(dpkg --print-architecture)" in
		amd64) arch=amd64 ;;
		arm64) arch=arm64 ;;
		*) die "no silo release for $(dpkg --print-architecture)" ;;
		esac
		apt_install curl ca-certificates
		base="${SILO_RELEASE_BASE:-https://github.com/bwsalmon/silo/releases/download}"
		url="$base/v$release/silo_${release}_linux_$arch"
		log "downloading $url"
		curl -fsSL -o "$tmp/silo" "$url"
		curl -fsSL -o "$tmp/silo.sha256" "$url.sha256"
		(cd "$tmp" && echo "$(awk '{print $1}' silo.sha256)  silo" | sha256sum -c --quiet -) ||
			die "checksum mismatch for $url"
		;;
	esac
	chmod 0755 "$tmp/silo"
	"$tmp/silo" help >/dev/null || die "the silo binary does not run"
	install -m 0755 -o root -g root "$tmp/silo" "$PREFIX_BIN/silo.new"
	mv -f "$PREFIX_BIN/silo.new" "$PREFIX_BIN/silo"
	log "installed $PREFIX_BIN/silo"
}

# ---- Node.js and the CLIs ------------------------------------------------

node_major() {
	command -v node >/dev/null 2>&1 || { echo 0; return; }
	node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0
}

install_node() {
	if [ "$(node_major)" -ge "$NODE_MAJOR" ]; then
		log "Node.js $(node --version) is new enough"
		return
	fi
	local cand
	apt_install ca-certificates curl gnupg
	cand="$(apt-cache policy nodejs 2>/dev/null | awk '/Candidate:/ {print $2}' | sed 's/^[0-9]*://; s/\..*//')"
	if [ -n "$cand" ] && [ "$cand" != "(none)" ] && [ "$cand" -ge "$NODE_MAJOR" ] 2>/dev/null; then
		apt_install nodejs npm
		return
	fi
	log "adding the NodeSource repository for Node.js $NODE_MAJOR"
	install -d -m 0755 /etc/apt/keyrings
	curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key |
		gpg --dearmor --yes -o /etc/apt/keyrings/nodesource.gpg
	echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_$NODE_MAJOR.x nodistro main" \
		>/etc/apt/sources.list.d/nodesource.list
	DEBIAN_FRONTEND=noninteractive apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs
	[ "$(node_major)" -ge "$NODE_MAJOR" ] || die "Node.js $NODE_MAJOR did not install"
}

install_clis() {
	install_node
	install -d -m 0755 -o root -g root "$CLI_DIR"
	log "installing the pinned CLIs into $CLI_DIR"
	# A private prefix with an exact lockfile-free install: --save-exact keeps
	# package.json honest about what is pinned.
	[ -f "$CLI_DIR/package.json" ] || echo '{"name":"silo-cli","private":true}' >"$CLI_DIR/package.json"
	npm install --prefix "$CLI_DIR" --save-exact --no-fund --no-audit --loglevel=error \
		"$CLAUDE_CODE_PKG" "$CLAUDE_ACP_PKG" "$CODEX_PKG" "$CODEX_ACP_PKG" "$GEMINI_CLI_PKG"
	chown -R root:root "$CLI_DIR"
	chmod -R go-w "$CLI_DIR"
	# Presets that point silo connect at the pinned binaries, whatever PATH
	# the agent has. They override the built-in presets of the same name.
	local bin="$CLI_DIR/node_modules/.bin"
	write_preset claude "$bin/claude-agent-acp"
	write_preset codex "$bin/codex-acp"
	write_preset gemini "$bin/gemini" --experimental-acp
}

write_preset() {
	local name="$1" f="$ETC/clis.d/$1.toml" cmd="" a
	shift
	for a in "$@"; do cmd="$cmd${cmd:+, }\"$a\""; done
	cat >"$f.new" <<EOF
# Written by install.sh: the pinned $name CLI in $CLI_DIR (versions.env).
name = "$name"
command = [$cmd]
EOF
	chmod 0644 "$f.new"
	mv -f "$f.new" "$f"
}

# ---- users, directories and files ----------------------------------------

ensure_group() { getent group "$1" >/dev/null || groupadd --system "$1"; }

# ensure_user NAME HOME COMMENT: a system user with a mode-0700 home.
ensure_user() {
	local name="$1" home="$2" comment="$3"
	if ! id -u "$name" >/dev/null 2>&1; then
		useradd --system --user-group --home-dir "$home" --no-create-home \
			--shell /usr/sbin/nologin --comment "$comment" "$name"
	fi
	install -d -m 0700 -o "$name" -g "$name" "$home"
	chown "$name:$name" "$home"
	chmod 0700 "$home"
}

setup_users_and_dirs() {
	ensure_group "$GW_GROUP"
	ensure_user "$BRIDGE_USER" "$STATE/bridge" "silo session bridge and web UI"
	usermod -a -G "$GW_GROUP" "$BRIDGE_USER"
	install -d -m 0755 -o root -g root "$STATE" "$STATE/home"
	ensure_user "$ROOT_USER" "$STATE/home/$ROOT_USER" "silo root agent"
	ensure_user "$USER_USER" "$STATE/home/$USER_USER" "silo default user agent"

	install -d -m 0755 -o root -g root "$ETC" "$ETC/clis.d" "$ETC/root.d" "$ETC/agents" "$SHARE"
	install -d -m 0700 -o root -g root "$ETC/secrets" "$STATE/gateway"
	install -d -m 0700 -o "$BRIDGE_USER" -g "$BRIDGE_USER" "$STATE/sessions" "$STATE/ui"
	install -d -m 0750 -o root -g root "$LOGDIR"

	# /run is a tmpfs: tmpfiles.d recreates /run/silo at boot. Sticky and
	# group-writable so the bridge (group silo) can create its socket but
	# cannot replace the gateway's.
	cat >/etc/tmpfiles.d/silo.conf <<EOF
# silo v3 (install.sh): sockets for the gateway and the session bridge.
d $RUNDIR 1775 root $BRIDGE_USER -
EOF
	systemd-tmpfiles --create /etc/tmpfiles.d/silo.conf

	"$PREFIX_BIN/silo" instructions >"$SHARE/root.md.new"
	chmod 0644 "$SHARE/root.md.new"
	mv -f "$SHARE/root.md.new" "$SHARE/root.md"

	if [ ! -e "$ETC/gateway.toml" ]; then
		log "writing $ETC/gateway.toml"
		cat >"$ETC/gateway.toml" <<EOF
# silo gateway configuration (design §11.2). Written once by install.sh;
# edit freely, install.sh never overwrites it.

# silo connect dials the session bridge, which relays to the gateway.
hub_url = "unix://$RUNDIR/bridge.sock"
bridge_user = "$BRIDGE_USER"
socket_group = "$GW_GROUP"

[root_agent]
user = "$ROOT_USER"
runtime = "$runtime"
permissions = "ask"

[user_agent]
name = "$USER_AGENT"
user = "$USER_USER"
runtime = "$runtime"
permissions = "ask"
EOF
		chmod 0644 "$ETC/gateway.toml"
	fi
}

# ---- systemd units -------------------------------------------------------

# agent_unit FILE AGENT USER DESCRIPTION EXTRA: silo connect as USER, with
# the registration token the gateway writes for AGENT.
agent_unit() {
	local file="$1" agent="$2" user="$3" desc="$4" extra="$5"
	cat >"$UNITDIR/$file" <<EOF
# Written by silo's install.sh; re-run it rather than editing this file.
[Unit]
Description=$desc
Documentation=file://$SHARE/root.md
After=silo-gateway.service silo-ui.service
Wants=silo-ui.service
# The gateway re-arms the token whenever the agent disconnects, so keep
# retrying for as long as it takes.
StartLimitIntervalSec=0

[Service]
Type=simple
User=$user
Group=$user
Environment=HOME=$STATE/home/$user
Environment=PATH=$CLI_DIR/node_modules/.bin:$PREFIX_BIN:/usr/local/bin:/usr/bin:/bin
Environment=SILO_HUB_URL=unix://$RUNDIR/bridge.sock
WorkingDirectory=$STATE/home/$user
# The gateway writes this agent's single-use registration token here;
# silo connect reads it from \$CREDENTIALS_DIRECTORY/token.
LoadCredential=token:$STATE/gateway/tokens/$agent
ExecStart=$PREFIX_BIN/silo connect
Restart=always
RestartSec=3
KillMode=mixed
TimeoutStopSec=40
MemoryMax=4G
CPUQuota=200%
TasksMax=1024
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
$extra

[Install]
WantedBy=multi-user.target
EOF
}

install_units() {
	log "installing systemd units"
	cat >"$UNITDIR/silo-gateway.service" <<EOF
# Written by silo's install.sh; re-run it rather than editing this file.
[Unit]
Description=silo gateway (agent registry, run_as_root, audit log)
Documentation=file://$SHARE/root.md
After=network.target

[Service]
Type=notify
ExecStart=$PREFIX_BIN/silo gateway -config $ETC/gateway.toml
Restart=on-failure
RestartSec=2
# Agents run outside the gateway and survive its restarts.
KillMode=process

[Install]
WantedBy=multi-user.target
Alias=silo-gatewayd.service
EOF

	cat >"$UNITDIR/silo-ui.service" <<EOF
# Written by silo's install.sh; re-run it rather than editing this file.
[Unit]
Description=silo session bridge and web UI
After=silo-gateway.service
Requires=silo-gateway.service

[Service]
Type=simple
User=$BRIDGE_USER
Group=$BRIDGE_USER
SupplementaryGroups=$GW_GROUP
ExecStart=$PREFIX_BIN/silo ui -http $listen -gateway-socket $RUNDIR/gateway.sock -socket $RUNDIR/bridge.sock -sessions-dir $STATE/sessions -token-file $UI_TOKEN_HASH
Restart=on-failure
RestartSec=2
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=$STATE/sessions $STATE/ui $RUNDIR

[Install]
WantedBy=multi-user.target
Alias=silo-bridge.service
EOF

	# The root agent's home and secrets are off limits to root's own CLI
	# beyond its home (design §7.2); its privileged path is run_as_root.
	agent_unit silo-root.service "$ROOT_AGENT" "$ROOT_USER" \
		"silo root agent ($ROOT_AGENT, silo connect as $ROOT_USER)" \
		"InaccessiblePaths=-$ETC/secrets -$STATE/gateway"
	agent_unit silo-user.service "$USER_AGENT" "$USER_USER" \
		"silo default user agent ($USER_AGENT, silo connect as $USER_USER)" \
		"InaccessiblePaths=-$ETC/secrets -$STATE/gateway -$STATE/home/$ROOT_USER"
	systemctl daemon-reload
}

# ---- UI login token --------------------------------------------------------

ui_token() {
	if [ "$rotate" -eq 1 ] || [ ! -s "$UI_TOKEN_HASH" ] || [ ! -s "$UI_TOKEN_RAW" ]; then
		log "generating the UI login token"
		local tok
		tok="$(runuser -u "$BRIDGE_USER" -- "$PREFIX_BIN/silo" ui token -file "$UI_TOKEN_HASH")"
		(umask 077 && printf '%s\n' "$tok" >"$UI_TOKEN_RAW.new")
		mv -f "$UI_TOKEN_RAW.new" "$UI_TOKEN_RAW"
		rotated=1
	fi
	chown root:root "$UI_TOKEN_RAW"
	chmod 0600 "$UI_TOKEN_RAW"
}

# ---- start -----------------------------------------------------------------

start_units() {
	log "starting silo"
	systemctl enable --quiet silo-gateway.service silo-ui.service silo-root.service silo-user.service
	systemctl restart silo-gateway.service
	systemctl restart silo-ui.service
	# Their tokens are re-armed when the old connection drops; Restart=
	# retries until they are there.
	systemctl restart silo-root.service silo-user.service || true
	local i ok=0
	for i in $(seq 1 30); do
		if "$PREFIX_BIN/silo" call agent.list 2>/dev/null | grep -q '"state": "connected"'; then
			local n
			n="$("$PREFIX_BIN/silo" call agent.list | grep -c '"state": "connected"' || true)"
			if [ "$n" -ge 2 ]; then ok=1; break; fi
		fi
		sleep 1
	done
	if [ "$ok" -eq 1 ]; then
		log "both standing agents ($ROOT_AGENT and $USER_AGENT) are connected"
	else
		log "warning: the standing agents have not both connected yet;"
		log "         see: journalctl -u silo-root -u silo-user -u silo-ui -u silo-gateway"
	fi
}

print_access() {
	local host port tok
	host="${listen%:*}"
	port="${listen##*:}"
	tok="$(cat "$UI_TOKEN_RAW")"
	cat <<EOF

silo is installed.

  The web UI listens on http://$listen (this box only).
  From your workstation, tunnel to it and open http://localhost:$port :

      ssh -L $port:$host:$port ${SUDO_USER:-$(id -un)}@$(hostname -f 2>/dev/null || hostname)

  UI login token${rotated:+ (new)}:

      $tok

  It is kept in $UI_TOKEN_RAW (root only); rotate it with
  install.sh --rotate-token.

  Agents: root (user $ROOT_USER, silo-root.service) and $USER_AGENT
  (user $USER_USER, silo-user.service). The human approval queue is in the
  UI, or: sudo silo approvals list
EOF
	if [ "$with_cli" -eq 1 ]; then
		cat <<EOF

  Model credentials: the agents' CLIs need them before they can answer.
  Put keys in $ETC/secrets/ (root, 0600) or log each CLI in as its agent user.
EOF
	fi
}

rotated=""
check_system
apt_install ca-certificates passwd
install_silo
setup_users_and_dirs
if [ "$with_cli" -eq 1 ]; then install_clis; fi
install_units
ui_token
if [ "$start" -eq 1 ]; then
	start_units
elif [ -n "$rotated" ]; then
	systemctl try-restart silo-ui.service || true
fi
print_access
