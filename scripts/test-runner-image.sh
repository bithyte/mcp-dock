#!/usr/bin/env bash
# Smoke test for the mcp-runner container image.
# Usage: scripts/test-runner-image.sh <image-ref>
# Env: CT (container CLI, default podman), EXPECT_VERSION (optional).
# Needs network access for npx and uvx.
set -u

IMAGE=${1:?usage: test-runner-image.sh <image-ref>}
CT=${CT:-podman}
EXPECT_VERSION=${EXPECT_VERSION:-}

# Pinned so the test doesn't drift; resolved 2026-11-12.
NPM_PKG=@modelcontextprotocol/server-everything@2.3.0
PIP_PKG=mcp-server-time==2026.11.11
TOKEN=test-runner-token

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
FAILED=0
PREFIX="mcp-runner-test-$$"
WORK=$(mktemp -d)
DATA="$WORK/data"
FAKE="$WORK/fake"
mkdir -p "$DATA" "$FAKE"
chmod 0755 "$FAKE"

# Every container gets the same hardening as a real deployment.
SECURE=(--read-only --cap-drop=ALL --security-opt no-new-privileges --user 65532:65532 --tmpfs /tmp)

# Run a command as root in a throwaway container with $DATA mounted at /d.
# The runner chmods what it creates to 0700 as uid 65532, so the host user
# can't read it afterwards (as on a docker runner).
# Usage: data_root <mount-opts> <cmd...>
data_root() {
	local opts=$1
	shift
	$CT run --rm --user 0 -v "$DATA:/d$opts" --entrypoint "$1" "$IMAGE" "${@:2}"
}

# shellcheck disable=SC2317
cleanup() {
	$CT rm -f "$PREFIX-npm" "$PREFIX-pip" "$PREFIX-bin" >/dev/null 2>&1
	data_root "" find /d -mindepth 1 -delete >/dev/null 2>&1
	rm -rf "$WORK"
}
trap cleanup EXIT

ok() { echo "ok $1"; }
fail() {
	echo "FAIL $1: $2"
	FAILED=1
}

# hostport <name>: the 127.0.0.1 address the container's 8080 is published on.
hostport() {
	local hp
	hp=$($CT port "$1" 8080/tcp 2>/dev/null | head -n1)
	echo "${hp/0.0.0.0/127.0.0.1}"
}

# wait_healthy <name>: poll /healthz until it answers ok or the container exits.
wait_healthy() {
	local i hp
	for ((i = 0; i < 60; i++)); do
		hp=$(hostport "$1")
		if [ -n "$hp" ] && [ "$(curl -s --max-time 5 "http://$hp/healthz")" = ok ]; then
			return 0
		fi
		if [ "$($CT inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" != true ]; then
			return 1
		fi
		sleep 1
	done
	return 1
}

# post <hostport> <session-id or ""> <token or ""> <json>: POST to /mcp,
# printing response headers then body. Initialize starts the child, which for
# npx and uvx means downloading the package, hence the long timeout.
post() {
	local args=(-s -i --max-time 300 -X POST "http://$1/mcp"
		-H 'Content-Type: application/json'
		-H 'Accept: application/json, text/event-stream'
		-H 'MCP-Protocol-Version: 2025-06-18')
	[ -n "$2" ] && args+=(-H "Mcp-Session-Id: $2")
	[ -n "$3" ] && args+=(-H "Authorization: Bearer $3")
	curl "${args[@]}" --data "$4"
}

# handshake <hostport> <token or "">: initialize, initialized, tools/list.
# Prints the session id on the first line, then the tools/list response.
handshake() {
	local out sid
	out=$(post "$1" "" "$2" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-runner-image","version":"0"}}}')
	sid=$(tr -d '\r' <<<"$out" | sed -n 's/^[Mm][Cc][Pp]-[Ss]ession-[Ii][Dd]: *//p' | head -n1)
	if [ -z "$sid" ]; then
		echo "no session id from initialize: $out"
		return 1
	fi
	echo "$sid"
	post "$1" "$sid" "$2" '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
	post "$1" "$sid" "$2" '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
}

# start <name> <extra run args...> -- <command...>
start() {
	local name=$1
	shift
	local extra=()
	while [ "$1" != -- ]; do
		extra+=("$1")
		shift
	done
	shift
	$CT run -d --name "$name" "${SECURE[@]}" -v "$DATA:/data" -p 127.0.0.1::8080 \
		"${extra[@]}" "$IMAGE" "$@" >/dev/null 2>&1
}

# 1. usage
# No command: the runner prints usage and exits 2.
out=$($CT run --rm --network none "${SECURE[@]}" "$IMAGE" 2>&1)
code=$?
if [ "$code" = 2 ] && grep -q 'usage: mcp-runner' <<<"$out"; then
	ok usage
else
	fail usage "exit $code: $out"
fi

# 2. uid
out=$($CT run --rm --entrypoint id "$IMAGE" -u 2>&1)
if [ "$out" = 65532 ]; then ok uid; else fail uid "got '$out'"; fi

# 3. version
if [ -n "$EXPECT_VERSION" ]; then
	out=$($CT run --rm --network none "${SECURE[@]}" "$IMAGE" -version 2>&1)
	if [ "$out" = "$EXPECT_VERSION" ]; then ok version; else fail version "got '$out', want '$EXPECT_VERSION'"; fi
fi

# 4. runtimes
bad=""
for c in node npx python3 uv uvx; do
	$CT run --rm --entrypoint "$c" "$IMAGE" --version >/dev/null 2>&1 || bad="$bad $c"
done
if [ -z "$bad" ]; then ok runtimes; else fail runtimes "failed:$bad"; fi

# 5. libatomic
out=$($CT run --rm --entrypoint ldconfig "$IMAGE" -p 2>&1)
if grep -q 'libatomic\.so\.1' <<<"$out"; then ok libatomic; else fail libatomic "not in ldconfig -p"; fi

# 6. uv_system_python
# shellcheck disable=SC2016
want=$($CT run --rm --entrypoint sh "$IMAGE" -c 'readlink -f "$(command -v python3)"' 2>&1)
got=$($CT run --rm "${SECURE[@]}" -e HOME=/tmp --entrypoint uv "$IMAGE" python find 2>&1)
if [ "$got" = /usr/bin/python3 ] || [ "$got" = "$want" ]; then
	ok uv_system_python
else
	fail uv_system_python "got '$got', want /usr/bin/python3 or '$want'"
fi

# 7. three_upstreams
# One container per upstream. They share one data dir, which a real deployment
# wouldn't need to, but it keeps the cache checks below simple.
if ! (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$FAKE/fakeupstream" ./internal/testutil/fakeupstream); then
	fail three_upstreams "building fakeupstream failed"
	exit 1
fi
# The host user doesn't own DATA from here on, so give it to 65532 up front.
if ! data_root "" chown 65532:65532 /d >/dev/null 2>&1; then
	fail three_upstreams "chown of data dir failed"
	exit 1
fi

# check_upstream <name> <token or ""> <tool>: handshake and look for <tool>.
check_upstream() {
	local hp out
	if ! wait_healthy "$1"; then
		echo "$1: not healthy, logs:"
		$CT logs "$1" 2>&1 | tail -n 20
		return 1
	fi
	hp=$(hostport "$1")
	out=$(handshake "$hp" "$2")
	if ! grep -q "\"name\":\"$3\"" <<<"$out"; then
		echo "$1: no tool $3 in tools/list: $out"
		$CT logs "$1" 2>&1 | tail -n 20
		return 1
	fi
}

start "$PREFIX-npm" -- npx --prefer-offline -y "$NPM_PKG" stdio
start "$PREFIX-pip" -- uvx "$PIP_PKG"
start "$PREFIX-bin" -e MCP_RUNNER_BEARER_TOKEN="$TOKEN" -v "$FAKE:/opt/fake:ro" -- /opt/fake/fakeupstream
bad=""
check_upstream "$PREFIX-npm" "" echo || bad="$bad npm"
check_upstream "$PREFIX-pip" "" get_current_time || bad="$bad pip"
check_upstream "$PREFIX-bin" "$TOKEN" getenv || bad="$bad bin"
if [ -z "$bad" ]; then ok three_upstreams; else fail three_upstreams "failed:$bad"; fi

# 8. bearer
hp=$(hostport "$PREFIX-bin")
init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}'
status() { post "$hp" "" "$1" "$init" | head -n1 | awk '{print $2}'; }
none=$(status "")
wrong=$(status wrong-token)
right=$(status "$TOKEN")
if [ "$none" = 401 ] && [ "$wrong" = 401 ] && [ "$right" = 200 ]; then
	ok bearer
else
	fail bearer "no header $none, wrong token $wrong, right token $right (want 401, 401, 200)"
fi

# 9. child_env
# The runner's own MCP_RUNNER_* vars must not reach the child.
out=$(handshake "$hp" "$TOKEN")
sid=$(head -n1 <<<"$out")
out=$(post "$hp" "$sid" "$TOKEN" '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"getenv","arguments":{"name":"MCP_RUNNER_BEARER_TOKEN"}}}')
# Go's encoder escapes < and > as \u003c and \u003e; accept either form.
if grep -qE '"text":"(<|\\u003c)unset(>|\\u003e)"' <<<"$out"; then
	ok child_env
else
	fail child_env "getenv MCP_RUNNER_BEARER_TOKEN: $out"
fi

# 10. healthz
# Without a token, and on the runner that has one: /healthz needs no auth.
out=$(curl -s --max-time 10 "http://$hp/healthz" 2>&1)
if [ "$out" = ok ]; then ok healthz; else fail healthz "got '$out' from $hp"; fi

# 11. cache_persists
# DATA is unreadable by the host, so list it from a root helper container.
snap_npm() { data_root ":ro" sh -c 'find /d/home/.npm/_cacache/content-v2 -type f | sort'; }
snap_uv() { data_root ":ro" sh -c 'find /d/home/.cache/uv/archive-v0 -mindepth 1 -maxdepth 1 | sort'; }
before_npm=$(snap_npm 2>&1)
before_uv=$(snap_uv 2>&1)
$CT stop -t 20 "$PREFIX-npm" "$PREFIX-pip" >/dev/null 2>&1
$CT start "$PREFIX-npm" "$PREFIX-pip" >/dev/null 2>&1
bad=""
check_upstream "$PREFIX-npm" "" echo || bad="$bad npm(restart)"
check_upstream "$PREFIX-pip" "" get_current_time || bad="$bad pip(restart)"
after_npm=$(snap_npm 2>&1)
after_uv=$(snap_uv 2>&1)
if [ -z "$before_npm" ] || [ "$before_npm" != "$after_npm" ]; then bad="$bad npm"; fi
if [ -z "$before_uv" ] || [ "$before_uv" != "$after_uv" ]; then bad="$bad uv"; fi
if [ -z "$bad" ]; then
	ok cache_persists
else
	fail cache_persists "failed:$bad"
	diff <(echo "$before_npm") <(echo "$after_npm") | head -n 10
	diff <(echo "$before_uv") <(echo "$after_uv") | head -n 10
fi

# 12. no_python_download
# Distinguish "absent" from "couldn't check": the helper prints a marker.
out=$(data_root ":ro" sh -c 'if [ -e /d/home/.local/share/uv/python ]; then echo present; else echo absent; fi' 2>&1)
case "$out" in
absent) ok no_python_download ;;
present) fail no_python_download "uv downloaded a python" ;;
*) fail no_python_download "couldn't check: $out" ;;
esac

exit $FAILED
