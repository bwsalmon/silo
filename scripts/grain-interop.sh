#!/usr/bin/env bash
# grain-interop: this silo commit's hub against a real grain daemon,
# built from a bwsalmon/grain checkout ($GRAIN_SRC, grain's main in
# .grain/workflows/grain-interop.yml). It checks the task interface
# (docs/hub-interface.md) as far as that grain supports it:
#
#   build    grain builds, and its hub tests pass, against this commit's
#            hub packages in place of the silo it pins;
#   login    `grain hub login` has the hub mint the node token (task 4172);
#   register grain registers as a node and shows online in GET /v1/nodes;
#   accept   a task filed through POST /v1/tasks is offered to grain and
#            accepted, and is a task on grain;
#   refuse   a task for a repository outside grain's local policy is
#            refused, and the hub excludes the node;
#   cancel   a cancel on the hub closes the task on grain;
#   result   a task that ends on grain is reported to the hub as failed.
#
# With a grain that checks the hub's signed claims (policy.ApplySiloClaims,
# docs/hub.md §6.4), the round trip runs again with signing on: the hub
# signs each offer with a P-256 key (-claims-issuer, -claims-key-file)
# and grain's policy names it as silo_issuer, with silo_keys_url its
# /v1/claims/keys. The same stages pass, and the accepted task's body is
# the prompt byte for byte, with no footer, which the signed spec_digest
# covers. A last check has a hub that signs nothing offer a task to that
# grain, which refuses it as "an offer with no Silo attestation".
#
# The hub runs on SQLite and signs alice in with OIDC against a fake
# provider (internal/fakeoidc); grain runs with a local policy, no
# sandbox backend and no CI, against a fake GitHub API
# (scripts/fake-github.py), so no task is ever dispatched, no agent or
# model runs and nothing reaches the network.
#
# The round trip runs twice: with grain as it is (the silo client it
# pins, which is what deployed grains speak) and with grain built
# against this commit (what grain's next bump of silo will speak). A
# stage grain does not support yet is skipped and says so; a stage it
# supports that fails is a failure. On a failure the same check is rerun
# with silo's base commit, and the last line says which side changed,
# by both commits' short SHAs.
#
# TODO(task 4203): when grain's own hub round-trip test lands, with a
# reusable harness, call that instead of keeping this second copy.
# TODO(task 4236): when the conformance kit lands, run it against the
# grain binary built here too.
# TODO: events (task 4174), view-through and merge need a task that
# runs, so a scripted agent in a sandbox; they are guarded and skipped
# until grain supports them and this job can drive one.
set -euo pipefail

SILO="$(cd "${SILO:-$(git rev-parse --show-toplevel)}" && pwd)"
GRAIN_SRC="$(cd "${GRAIN_SRC:?GRAIN_SRC: a bwsalmon/grain checkout to build grain from}" && pwd)"
W="${WORK_DIR:-$(mktemp -d /tmp/grain-interop.XXXXXX)}"
mkdir -p "$W/bin"

silo_sha="$(git -C "$SILO" rev-parse --short=12 HEAD)"
grain_sha="$(git -C "$GRAIN_SRC" rev-parse --short=12 HEAD)"

USER_EMAIL=alice@interop.test
NODE=interop-grain
HUB_PORT=18080
UI_PORT=14242
GITHUB_PORT=18999
OIDC_PORT=18998
ID_TOKEN=""
# Every wait below is bounded; grain's hub client polls a running task's
# state every 15s (pkg/hubnode DefaultPoll).
WAIT=90

say() { printf 'grain-interop: %s\n' "$*"; }
stage() { printf '\n=== %s\n' "$*"; }

# ---- builds ---------------------------------------------------------

# build_silo DIR OUT builds silo's command from the checkout DIR.
build_silo() {
	(cd "$1" && go build -o "$2" ./cmd/silo)
}

# grain_silo_import is the silo module grain's go.mod requires.
grain_silo_import() {
	(cd "$GRAIN_SRC" && go list -m -f '{{.Path}}' all 2>/dev/null | grep -E '^github.com/bwsalmon/silo(/v[0-9]+)?$' | head -1)
}

# build_grain_pinned OUT builds grain as grain's main has it. If grain's
# tip commit on main does not compile on its own (an upstream break in
# bwsalmon/grain unrelated to silo) or has removed the silo dependency, step
# back to the most recent commit on grain's main that requires silo and builds.
build_grain_pinned() {
	if [ -n "$(grain_silo_import)" ] && (cd "$GRAIN_SRC" && go build -o "$1" ./cmd/grain) 2>"$W/pinned.err"; then
		return 0
	fi
	if [ -z "$(grain_silo_import)" ]; then
		say "NOTICE: grain main ($grain_sha) has no github.com/bwsalmon/silo dependency; stepping back to the last commit that did"
	else
		say "NOTICE: grain main ($grain_sha) does not build on its own:"
		sed 's/^/  /' "$W/pinned.err"
	fi
	git -C "$GRAIN_SRC" fetch -q --deepen=50 origin main 2>/dev/null || true
	local rev
	for rev in $(git -C "$GRAIN_SRC" rev-list --first-parent --max-count=50 HEAD~1 2>/dev/null); do
		git -C "$GRAIN_SRC" checkout -q "$rev"
		if [ -n "$(grain_silo_import)" ] && (cd "$GRAIN_SRC" && go build -o "$1" ./cmd/grain) 2>/dev/null; then
			grain_sha="$(git -C "$GRAIN_SRC" rev-parse --short=12 HEAD)"
			say "NOTICE: stepped back to grain $grain_sha ($(git -C "$GRAIN_SRC" log -1 --format=%s HEAD))"
			return 0
		fi
	done
	if [ -z "$(grain_silo_import)" ]; then
		say "skipped: no recent grain commit requires github.com/bwsalmon/silo"
		exit 0
	fi
	cat "$W/pinned.err" >&2
	return 1
}

# build_grain_against SILODIR OUT builds grain with its silo dependency
# replaced by the checkout SILODIR. Where grain imports silo under a
# different module path than SILODIR declares (silo/v2 before silo moved
# its module to the repository root), the imports are rewritten first:
# the same mechanical change grain's next bump of silo makes.
build_grain_against() {
	local silodir="$1" out="$2" tree
	tree="$(mktemp -d "$W/grain-tree.XXXXXX")"
	git -C "$GRAIN_SRC" archive HEAD | tar -x -C "$tree"
	local want have
	want="$(cd "$silodir" && go list -m)"
	have="$(grain_silo_import)"
	if [ -z "$have" ]; then
		echo "grain's go.mod requires no github.com/bwsalmon/silo module" >&2
		return 1
	fi
	(
		cd "$tree"
		if [ "$have" != "$want" ]; then
			grep -rlF --include='*.go' "\"$have/" . | xargs -r sed -i "s#\"$have/#\"$want/#g"
			go mod edit -droprequire "$have"
		fi
		go mod edit -require "$want@v0.0.0-00010101000000-000000000000" -replace "$want=$silodir"
		GOFLAGS=-mod=mod go mod tidy >/dev/null 2>"$tree.tidy" || { cat "$tree.tidy" >&2; return 1; }
		go build -o "$out" ./cmd/grain
		go vet ./pkg/hubnode ./pkg/daemon
		go test -count=1 ./pkg/hubnode
		go test -count=1 -run Hub ./pkg/daemon
	)
}

# ---- what grain supports -------------------------------------------

# grain_has PATTERN is whether grain's hub node code matches: pkg/hubnode
# and pkg/daemon's hub*.go, tests aside.
grain_has() {
	find "$GRAIN_SRC/pkg/hubnode" "$GRAIN_SRC/pkg/daemon" -name '*.go' ! -name '*_test.go' \
		\( -path '*/pkg/hubnode/*' -o -name 'hub*.go' \) -print0 2>/dev/null |
		xargs -0 -r grep -qE "$1"
}

# ---- HTTP -----------------------------------------------------------

hub() { # METHOD PATH [BODY]
	local args=(-sS -H "Authorization: Bearer $ID_TOKEN" -X "$1" "http://127.0.0.1:$HUB_PORT$2")
	[ $# -ge 3 ] && args+=(-H 'Content-Type: application/json' -d "$3")
	curl "${args[@]}"
}
# GRAIN_CODE is the running grain's access code file. A single-tenant
# grain asks every caller, loopback included, for the code it writes
# there; one that writes none (older grains) trusts loopback.
GRAIN_CODE=
grain_auth() {
	[ -n "$GRAIN_CODE" ] && [ -s "$GRAIN_CODE" ] && printf '%s\n' -H "Authorization: Bearer $(tr -d '[:space:]' <"$GRAIN_CODE")"
}
grainapi() { # METHOD PATH [BODY]
	local args=(-sS -X "$1" "http://127.0.0.1:$UI_PORT$2")
	local -a auth; mapfile -t auth < <(grain_auth)
	args+=("${auth[@]}")
	[ $# -ge 3 ] && args+=(-H 'Content-Type: application/json' -d "$3")
	curl "${args[@]}"
}
grain_up() {
	local -a auth; mapfile -t auth < <(grain_auth)
	curl -sf "${auth[@]}" "http://127.0.0.1:$UI_PORT/api/deployment" -o /dev/null
}

# until_true SECONDS CMD... runs CMD every second until it succeeds.
until_true() {
	local deadline=$((SECONDS + $1)); shift
	until "$@"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 1
	done
}

# ---- one round trip -------------------------------------------------

PIDS=()
stop_all() {
	local p
	for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
	for p in "${PIDS[@]}"; do wait "$p" 2>/dev/null || true; done
	PIDS=()
}
trap stop_all EXIT

FAILED_STAGE=""
FAILED_WHY=""
fail() { FAILED_STAGE="$1"; FAILED_WHY="$2"; return 1; }

node_online() {
	hub GET /v1/nodes | jq -e --arg n "$NODE" '.nodes[] | select(.id == $n and .status == "online")' >/dev/null
}
task_field() { hub GET "/v1/tasks/$1" | jq -r ".$2 // empty"; }
task_settled() { # accepted (node_task set) or refused by NODE
	local t; t="$(hub GET "/v1/tasks/$1")"
	echo "$t" | jq -e --arg n "$NODE" '(.node_task // "") != "" or ((.excluded // []) | index($n))' >/dev/null
}
task_state_is() { [ "$(task_field "$1" state)" = "$2" ]; }

# PROMPT is every hub task's prompt.
PROMPT="grain-interop: nothing to do"

# file_task REPO TITLE prints the hub task's ID.
file_task() {
	local out
	out="$(hub POST /v1/tasks "$(jq -nc --arg r "$1" --arg t "$2" --arg p "$PROMPT" '{repo: $r, title: $t, prompt: $p}')")"
	echo "$out" | jq -er .id || { echo "POST /v1/tasks: $out" >&2; return 1; }
}

# grain_team ID REPO gives the grain daemon a team owning REPO, alice in
# it, with a git connection, which grain wants before it files a task
# for a repository: a token for the deployment's GitHub, which
# -github-host makes the fake one (scripts/fake-github.py), so grain can
# say who the token is and that it may push to REPO without reaching
# GitHub. Nothing here dispatches, so nothing ever clones with it.
grain_team() {
	local id="$1" repo="$2" out cur users target_repos
	cur="$(grainapi GET /api/deployment 2>/dev/null || true)"
	users="$(echo "$cur" | jq -c --arg u "$USER_EMAIL" '((.users // []) + [$u]) | unique' 2>/dev/null)" || users="$(jq -nc --arg u "$USER_EMAIL" '[$u]')"
	target_repos="$(echo "$cur" | jq -c --arg r "$repo" '((.targetRepos // []) + [$r]) | unique' 2>/dev/null)" || target_repos="$(jq -nc --arg r "$repo" '[$r]')"
	out="$(grainapi PUT /api/deployment "$(jq -nc --arg id "$id" --argjson u "$users" --argjson r "$target_repos" \
		'{id: $id, name: $id, users: $u, maxWorkers: 1, targetRepos: $r}')")"
	grainapi GET /api/deployment | jq -e --arg r "$repo" '.targetRepos | index($r)' >/dev/null ||
		{ fail setup "could not give grain a team for $repo: $out"; return 1; }
	if ! grainapi GET /api/deployment/git-connections |
		jq -e --arg h "127.0.0.1:$GITHUB_PORT" 'arrays | .[] | select(.host == $h and .status.configured and (.status.commitError // "") == "")' >/dev/null 2>&1; then
		out="$(grainapi POST /api/deployment/git-connections "$(jq -nc --arg h "127.0.0.1:$GITHUB_PORT" \
			'{host: $h, baseUrl: ("http://" + $h), kind: "github", value: "grain-interop"}')")"
		grainapi GET /api/deployment/git-connections |
			jq -e 'arrays | .[] | select(.status.configured and (.status.commitError // "") == "")' >/dev/null ||
			{ fail setup "could not give grain's team $id a git connection: $out"; return 1; }
	fi
}

# claims_key FILE writes a P-256 private key for the hub to sign claims
# with (docs/hub.md §6.4): openssl's, or Go's where there is no openssl.
claims_key() {
	if command -v openssl >/dev/null; then
		openssl ecparam -name prime256v1 -genkey -noout -out "$1"
		return
	fi
	mkdir -p "$W/keygen"
	cat >"$W/keygen/main.go" <<-'EOF'
		package main

		import (
			"crypto/ecdsa"
			"crypto/elliptic"
			"crypto/rand"
			"crypto/x509"
			"encoding/pem"
			"os"
		)

		func main() {
			k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				panic(err)
			}
			der, err := x509.MarshalECPrivateKey(k)
			if err != nil {
				panic(err)
			}
			pem.Encode(os.Stdout, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		}
	EOF
	(cd "$W/keygen" && go run main.go) >"$1"
}

# round_trip LABEL SILOBIN GRAINBIN [CLAIMS] runs every stage, stopping
# at the first failure, which it leaves in FAILED_STAGE and FAILED_WHY.
# CLAIMS is empty for grain's policy as it was before signed claims;
# "signed" for a hub that signs its offers and a grain whose policy
# names it as silo_issuer; "unsigned" for that grain against a hub that
# signs nothing, which runs only as far as the offer it refuses.
round_trip() {
	local label="$1" silo="$2" grain="$3" claims="${4:-}"
	local d="$W/$label"
	rm -rf "$d"; mkdir -p "$d/grain"
	FAILED_STAGE="" FAILED_WHY=""
	say "round trip $label: $silo against $grain${claims:+ (claims $claims)}"

	# The hub signs humans in with OIDC, against a fake provider
	# (internal/fakeoidc) that writes alice's ID token, which every call
	# below presents as a bearer token -- and `grain hub login` too.
	"$W/bin/fakeoidc" -addr "127.0.0.1:$OIDC_PORT" -client-id grain-interop -email "$USER_EMAIL" -token-file "$d/id-token" >"$d/oidc.log" 2>&1 &
	PIDS+=($!)
	until_true 10 test -s "$d/id-token" || { fail setup "the fake OIDC provider did not come up: $(cat "$d/oidc.log")"; return 1; }
	ID_TOKEN="$(cat "$d/id-token")"

	# Signed claims: the hub signs as its own URL, and grain fetches
	# its keys there; grain takes plain http only from a loopback host.
	local issuer="http://127.0.0.1:$HUB_PORT"
	local -a signing=()
	if [ "$claims" = signed ]; then
		claims_key "$d/claims-key.pem" 2>"$d/setup.err" || { fail setup "a P-256 key for the hub's claims: $(cat "$d/setup.err")"; return 1; }
		signing=(-claims-issuer "$issuer" -claims-key-file "$d/claims-key.pem")
	fi

	local db="sqlite:$d/hub.db"
	"$silo" hub team -database "$db" -id default -name Default -admin "user:$USER_EMAIL" -repo acme/widgets -repo acme/outside >/dev/null 2>"$d/setup.err" ||
		{ fail setup "silo hub team: $(cat "$d/setup.err")"; return 1; }
	SILO_HUB_OIDC_SESSION_KEY=grain-interop-session-key-of-32-bytes-or-more \
		"$silo" hub serve -dev -addr "127.0.0.1:$HUB_PORT" -database "$db" -admins user:admin@interop.test \
		-oidc-issuer "http://127.0.0.1:$OIDC_PORT" -oidc-client-id grain-interop \
		-oidc-redirect-url "http://127.0.0.1:$HUB_PORT/auth/callback" "${signing[@]}" >"$d/hub.log" 2>&1 &
	PIDS+=($!)
	until_true 30 curl -sf "http://127.0.0.1:$HUB_PORT/readyz" -o /dev/null || { fail hub "the hub did not come up"; return 1; }
	if [ "$claims" = signed ]; then
		curl -sf "$issuer/v1/claims/keys" | jq -e '.keys | length > 0' >/dev/null ||
			{ fail hub "the hub publishes no claims keys at $issuer/v1/claims/keys"; return 1; }
	fi

	# The node token: `grain hub login` has the hub mint it (task 4172);
	# a grain without it is given one `silo hub token` mints.
	stage "$label: login"
	if grep -qs '"login"' "$GRAIN_SRC/cmd/grain/hub.go"; then
		"$grain" hub login -hub-url "http://127.0.0.1:$HUB_PORT" -id-token-file "$d/id-token" \
			-data-dir "$d/grain" -hub-node-id "$NODE" -hub-token-file "$d/token" >"$d/login.log" 2>&1 ||
			{ fail login "grain hub login: $(cat "$d/login.log")"; return 1; }
		[ -s "$d/token" ] || { fail login "grain hub login wrote no node token: $(cat "$d/login.log")"; return 1; }
		say "grain hub login: $(tail -1 "$d/login.log")"
		say "the hub's node tokens: $(hub GET /v1/node-tokens | head -c 400)"
	else
		"$silo" hub token -database "$db" -owner "$USER_EMAIL" -node "$NODE" >"$d/token" 2>"$d/setup.err" ||
			{ fail setup "silo hub token: $(cat "$d/setup.err")"; return 1; }
		say "skipped: grain $grain_sha has no \`grain hub login\` (task 4172); the node token is silo hub token's"
	fi

	# A fake GitHub API (scripts/fake-github.py).
	python3 "$SILO/scripts/fake-github.py" "$GITHUB_PORT" >"$d/github.log" 2>&1 &
	PIDS+=($!)
	until_true 10 curl -sf "http://127.0.0.1:$GITHUB_PORT/user" -o /dev/null || { fail setup "the fake GitHub did not come up"; return 1; }

	# The local policy: acme/widgets only, and alice is this grain's
	# owner. acme/outside is the hub team's too, and registered, but
	# not grain's.
	cat >"$d/policy.yml" <<-EOF
		repos: [acme/widgets]
		local_requesters: [$USER_EMAIL]
	EOF
	# Under silo_issuer every hub task is external, and grain checks its
	# claims: iss this hub, aud grain's -hub-node-id (silo_audience is
	# left to default to it, which is the aud the hub signs). The hub has
	# no approval flow yet (docs/hub.md §6.4), so approved_by is always
	# empty and grain is told to accept unapproved external tasks.
	# TODO: when the hub can approve a task, have a second user approve
	# it, make them a member of grain's widgets team, and drop
	# allow_unapproved_external, so approved_by is checked too.
	if [ -n "$claims" ]; then
		cat >>"$d/policy.yml" <<-EOF
			silo_issuer: $issuer
			silo_keys_url: $issuer/v1/claims/keys
			allow_unapproved_external: true
		EOF
	fi
	# No sandbox backend, no CI and GitHub nowhere: nothing here runs
	# an agent or reaches the network.
	"$grain" daemon -data-dir "$d/grain" -ui-addr "127.0.0.1:$UI_PORT" -as "$USER_EMAIL" -ci=false -disable-native-sandboxes \
		-github-host "127.0.0.1:$GITHUB_PORT" -github-insecure-http -policy "$d/policy.yml" \
		-hub-url "http://127.0.0.1:$HUB_PORT" -hub-token-file "$d/token" -hub-node-id "$NODE" \
		-hub-repos acme/widgets,acme/outside -hub-slots 4 >"$d/grain.log" 2>&1 &
	PIDS+=($!)
	GRAIN_CODE="$d/grain/local-api-token"
	until_true 60 grain_up || { fail grain "grain's API did not come up"; return 1; }

	grain_team default acme/widgets || return 1

	stage "$label: register"
	until_true "$WAIT" node_online || { fail register "grain did not show online in GET /v1/nodes: $(hub GET /v1/nodes)"; return 1; }
	hub GET /v1/nodes | jq -c '.nodes[]'

	if [ "$claims" = unsigned ]; then
		stage "$label: refuse unsigned"
		# A hub given no -claims-* signs nothing, so its offer carries
		# no requester_attestation, and a grain whose policy names
		# silo_issuer refuses it.
		local u why
		u="$(file_task acme/widgets 'interop: unsigned')" || { fail unsigned "the hub refused the task"; return 1; }
		until_true "$WAIT" task_settled "$u" || { fail unsigned "hub task $u was neither accepted nor refused: $(hub GET "/v1/tasks/$u")"; return 1; }
		if [ -n "$(task_field "$u" node_task)" ]; then
			fail unsigned "grain, under silo_issuer, accepted hub task $u, which a hub that signs nothing offered with no attestation (as grain task $(task_field "$u" node_task))"
			return 1
		fi
		why="$(task_field "$u" reason)"
		case "$why" in
		*"an offer with no Silo attestation"*) ;;
		*) fail unsigned "grain refused hub task $u, an offer with no attestation, but not for that: $why"; return 1 ;;
		esac
		say "hub task $u refused: $(hub GET "/v1/tasks/$u" | jq -c '{state, reason, excluded}')"
		stop_all
		say "round trip $label: ok"
		return 0
	fi

	stage "$label: accept"
	local a na
	a="$(file_task acme/widgets 'interop: accept')" || { fail accept "the hub refused the task"; return 1; }
	until_true "$WAIT" task_settled "$a" || { fail accept "hub task $a was neither accepted nor refused: $(hub GET "/v1/tasks/$a")"; return 1; }
	na="$(task_field "$a" node_task)"
	[ -n "$na" ] || { fail accept "grain did not accept hub task $a: $(hub GET "/v1/tasks/$a" | jq -c '{state, reason, excluded}')"; return 1; }
	[ "$(task_field "$a" node)" = "$NODE" ] || { fail accept "hub task $a placed on $(task_field "$a" node), not $NODE"; return 1; }
	grainapi GET "/api/tasks/$na" | jq -e '.title == "interop: accept"' >/dev/null ||
		{ fail accept "grain accepted hub task $a as task $na, which grain does not have: $(grainapi GET "/api/tasks/$na")"; return 1; }
	if [ "$claims" = signed ]; then
		# The signed spec_digest covers the body, so grain files the
		# prompt byte for byte, with no "Placed here by the Silo hub"
		# footer: a note grain adds says so in a comment instead.
		local ta
		ta="$(grainapi GET "/api/tasks/$na")"
		echo "$ta" | jq -e --arg p "$PROMPT" '(.description // .body) == $p' >/dev/null ||
			{ fail accept "grain task $na, hub task $a under signed claims, has a body that is not the prompt byte for byte (a footer?): $ta"; return 1; }
		say "hub task $a's signed claims verified: grain task $na's body is the prompt, with no footer"
	fi
	say "hub task $a accepted as grain task $na ($(task_field "$a" state))"

	stage "$label: refuse"
	# acme/outside is the hub team's, and grain registers it, but its
	# local policy allows only acme/widgets. A grain that checks offers
	# against that policy is given a team for acme/outside too, so the
	# policy is all that refuses it; one that does not yet refuses it
	# only because no team of its own owns the repository.
	local b why
	if grain_has '(policy|Policy)\.[A-Z]|\.Policy\b|[pP]olicy\('; then
		grain_team default acme/outside || return 1
		why="its local policy does not allow"
	else
		say "NOTICE: grain $grain_sha does not check an offer against its local policy (-policy): it holds the task at dispatch instead, so this checks the refusal of a repository no team of grain's owns"
		why="no team of grain's owns"
	fi
	b="$(file_task acme/outside 'interop: refuse')" || { fail refuse "the hub refused the task"; return 1; }
	until_true "$WAIT" task_settled "$b" || { fail refuse "hub task $b was neither accepted nor refused: $(hub GET "/v1/tasks/$b")"; return 1; }
	if [ -n "$(task_field "$b" node_task)" ]; then
		fail refuse "grain accepted hub task $b for acme/outside, which $why (as grain task $(task_field "$b" node_task))"
		return 1
	fi
	say "hub task $b refused: $(hub GET "/v1/tasks/$b" | jq -c '{state, reason, excluded}')"

	stage "$label: cancel"
	if task_state_is "$a" running; then
		hub POST "/v1/tasks/$a/cancel" '{"reason":"grain-interop"}' >/dev/null
		grain_closed() { grainapi GET "/api/tasks/$na" | jq -e '.state == "closed"' >/dev/null; }
		until_true "$WAIT" grain_closed || { fail cancel "hub task $a cancelled, but grain task $na is $(grainapi GET "/api/tasks/$na" | jq -r .state)"; return 1; }
		say "hub task $a cancelled; grain task $na closed"
	else
		fail cancel "hub task $a is $(task_field "$a" state) before it was cancelled: $(hub GET "/v1/tasks/$a" | jq -c '{state, reason}')"
		return 1
	fi

	stage "$label: result"
	local c nc
	c="$(file_task acme/widgets 'interop: result')" || { fail result "the hub refused the task"; return 1; }
	until_true "$WAIT" task_settled "$c" || { fail result "hub task $c was not settled: $(hub GET "/v1/tasks/$c")"; return 1; }
	nc="$(task_field "$c" node_task)"
	[ -n "$nc" ] || { fail result "grain did not accept hub task $c: $(task_field "$c" reason)"; return 1; }
	GRAIN_SERVER="http://127.0.0.1:$UI_PORT" GRAIN_DATA_DIR="$d/grain" "$grain" close "$nc" >/dev/null 2>"$d/close.err" ||
		{ fail result "closing grain task $nc: $(cat "$d/close.err")"; return 1; }
	until_true "$WAIT" task_state_is "$c" failed || { fail result "grain task $nc closed, but hub task $c is $(task_field "$c" state)"; return 1; }
	say "grain task $nc closed; hub task $c failed: $(task_field "$c" reason)"

	# Stages grain does not support yet, each guarded by a look at
	# grain's own hub node code.
	stage "$label: events"
	if grain_has 'hub/live|EventsPath|events\.Event'; then
		say "NOTICE: grain $grain_sha streams events (task 4174), but this job has no task that runs to stream them: extend it with a scripted agent"
	else
		say "skipped: grain $grain_sha does not stream events to the hub yet (task 4174)"
	fi
	stage "$label: view-through"
	if grain_has 'TypeView|"view"'; then
		say "NOTICE: grain $grain_sha serves views, but this job has no artifact to view: extend it"
	else
		say "skipped: grain $grain_sha does not serve view streams yet (hub.md §7.2)"
	fi
	stage "$label: merge"
	say "skipped: grain $grain_sha reports no merge to the hub (the interface has no merge message yet)"

	stop_all
	say "round trip $label: ok"
}

# ---- attribution ----------------------------------------------------

# silo_base is the silo commit this one is compared against when the
# check fails: main's tip, or the commit before it when this is main.
silo_base() {
	local base
	git -C "$SILO" fetch -q origin main 2>/dev/null || true
	base="$(git -C "$SILO" rev-parse -q --verify FETCH_HEAD || true)"
	if [ -z "$base" ] || [ "$base" = "$(git -C "$SILO" rev-parse HEAD)" ]; then
		git -C "$SILO" fetch -q --deepen=1 origin 2>/dev/null || true
		base="$(git -C "$SILO" rev-parse -q --verify HEAD^1 || true)"
	fi
	echo "$base"
}

# blame KIND RERUN... reruns a failed check against silo's base commit
# and says which side changed. RERUN is called with the base checkout's
# directory.
blame() {
	local what="$1"; shift
	local base base_sha
	base="$(silo_base)"
	echo
	if [ -z "$base" ]; then
		say "FAIL: $what with silo $silo_sha and grain $grain_sha; no silo base commit to compare against, so which side changed is unknown"
		return
	fi
	base_sha="$(git -C "$SILO" rev-parse --short=12 "$base")"
	if [ ! -d "$W/silo-base" ]; then
		git -C "$SILO" worktree prune
		git -C "$SILO" worktree add -q --detach "$W/silo-base" "$base"
	fi
	say "rerunning against silo's base $base_sha to see which side changed"
	if "$@" "$W/silo-base" >"$W/base.log" 2>&1; then
		say "FAIL: $what. SILO CHANGED: silo $silo_sha breaks it against grain $grain_sha, and silo's base $base_sha passes against the same grain"
	else
		say "FAIL: $what. GRAIN CHANGED (or silo's base was already broken): grain $grain_sha fails against silo $silo_sha and against silo's base $base_sha alike"
		tail -20 "$W/base.log" | sed 's/^/  base: /'
	fi
}

dump_logs() {
	local d="$W/$1" f
	for f in hub.log login.log grain.log github.log; do
		[ -f "$d/$f" ] || continue
		echo "--- $1/$f (last 60 lines)"
		grep -vE 'orchestrator: seeded|package cache|Actions cache|disk floor' "$d/$f" | tail -60
	done
}

# rerun_build BASEDIR / rerun_trip LABEL GRAINMODE CLAIMS BASEDIR are
# what blame reruns against silo's base.
rerun_build() { build_grain_against "$1" "$W/bin/grain-base"; }
rerun_trip() {
	local label="$1" mode="$2" claims="$3" basedir="$4" g
	build_silo "$basedir" "$W/bin/silo-base"
	case "$mode" in
	pinned) g="$W/bin/grain-pinned" ;;
	*) build_grain_against "$basedir" "$W/bin/grain-base" && g="$W/bin/grain-base" ;;
	esac
	round_trip "$label-base" "$W/bin/silo-base" "$g" "$claims"
}

# ---- main -----------------------------------------------------------

say "silo $silo_sha against grain $grain_sha ($(git -C "$GRAIN_SRC" log -1 --format=%s HEAD))"
status=0

stage "build"
build_silo "$SILO" "$W/bin/silo"
(cd "$SILO" && go build -o "$W/bin/fakeoidc" ./internal/fakeoidc)
build_grain_pinned "$W/bin/grain-pinned"
say "grain pins $(grain_silo_import) $(cd "$GRAIN_SRC" && go list -m -f '{{.Version}}' "$(grain_silo_import)")"
if build_grain_against "$SILO" "$W/bin/grain-head" 2>"$W/build.err"; then
	say "grain $grain_sha builds and its hub tests pass against silo $silo_sha"
	trips="pinned head"
else
	cat "$W/build.err"
	blame "grain $grain_sha does not build (or its hub tests fail) against silo $silo_sha's packages" rerun_build
	status=1
	trips="pinned"
fi

# Signed claims (docs/hub.md §6.4) run once grain checks them on an
# offer: its hubTasks.Start passes the attestation to
# policy.ApplySiloClaims (grain task 4449).
claims_trips=""
if grain_has 'ApplySiloClaims'; then
	claims_trips="signed unsigned"
else
	say "NOTICE: grain $grain_sha does not check the hub's signed claims on an offer yet (policy.ApplySiloClaims in pkg/daemon, grain task 4449): the round trip with signing on is skipped"
fi

for t in $trips; do
	g="$W/bin/grain-$t"
	for c in "" $claims_trips; do
		label="$t${c:+-$c}"
		if ! round_trip "$label" "$W/bin/silo" "$g" "$c"; then
			dump_logs "$label"
			stop_all
			case "$FAILED_STAGE" in
			hub | grain | setup)
				say "FAIL: this job could not set up the round trip ($FAILED_STAGE): $FAILED_WHY. That is grain's (or the hub's) own configuration API, not the task interface; grain is $grain_sha, silo $silo_sha"
				status=1
				continue
				;;
			esac
			blame "stage $FAILED_STAGE of the round trip${c:+ with $c claims} with grain ${t/head/built against this silo}: $FAILED_WHY" rerun_trip "$label" "$t" "$c"
			status=1
		fi
		stop_all
	done
done

exit "$status"
