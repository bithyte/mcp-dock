#!/usr/bin/env bash
# Smoke test for the mcp-dock container image.
# Usage: scripts/test-image.sh <image-ref>
# Env: CT (container CLI, default podman), EXPECT_VERSION (optional).
set -u

IMAGE=${1:?usage: test-image.sh <image-ref>}
CT=${CT:-podman}
EXPECT_VERSION=${EXPECT_VERSION:-}

# Pinned so the test doesn't drift; resolved 2026-11-12.
NPM_PKG=@modelcontextprotocol/server-everything@2.3.0
PIP_PKG=mcp-server-time==2026.11.11

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
FAILED=0
CNAME="mcp-dock-test-$$"
WORK=$(mktemp -d)
DATA="$WORK/data"
FAKE="$WORK/fake"
mkdir -p "$DATA" "$FAKE"
chmod 0777 "$DATA"

USERNS=()
if [ "$CT" = podman ] && [ "$($CT info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" = true ]; then
	USERNS=("--userns=keep-id:uid=65532,gid=65532")
fi

# shellcheck disable=SC2317
cleanup() {
	$CT rm -f "$CNAME" >/dev/null 2>&1
	$CT run --rm --user 0 --entrypoint rm -v "$DATA:/d" "$IMAGE" \
		-rf /d/home /d/gateway.db /d/upstreams >/dev/null 2>&1
	rm -rf "$WORK"
}
trap cleanup EXIT

ok() { echo "ok $1"; }
fail() {
	echo "FAIL $1: $2"
	FAILED=1
}

# 1. usage
# CMD is serve, which needs a config, so call the entrypoint bare to get the usage text.
out=$($CT run --rm --network none --entrypoint mcp-dock "$IMAGE" 2>&1)
if grep -q 'mcp-dock serve' <<<"$out"; then ok usage; else fail usage "$out"; fi

# 2. uid
out=$($CT run --rm --entrypoint id "$IMAGE" -u 2>&1)
if [ "$out" = 65532 ]; then ok uid; else fail uid "got '$out'"; fi

# 3. version
if [ -n "$EXPECT_VERSION" ]; then
	out=$($CT run --rm --entrypoint mcp-dock "$IMAGE" version 2>&1)
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
got=$($CT run --rm --read-only --tmpfs /tmp --user 65532:65532 -e HOME=/tmp \
	--entrypoint uv "$IMAGE" python find 2>&1)
if [ "$got" = /usr/bin/python3 ] || [ "$got" = "$want" ]; then
	ok uv_system_python
else
	fail uv_system_python "got '$got', want /usr/bin/python3 or '$want'"
fi

# 7. three_upstreams
if ! (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$FAKE/fakeupstream" ./internal/testutil/fakeupstream); then
	fail three_upstreams "building fakeupstream failed"
fi
cat >"$DATA/config.yaml" <<YAML
public_url: http://localhost:8080
listen: ":8080"
data_dir: /data
github:
  client_id: test
  client_secret_env: GITHUB_CLIENT_SECRET
allowed_users: [test]
upstreams:
  - name: npm
    command: npx
    args: ["--prefer-offline", "-y", "$NPM_PKG", "stdio"]
  - name: pip
    command: uvx
    args: ["$PIP_PKG"]
  - name: bin
    command: /opt/fake/fakeupstream
YAML
chmod 0644 "$DATA/config.yaml"

# Optional $1: number of log lines to skip (earlier runs of the same container).
wait_started() {
	local i logs
	for ((i = 0; i < 300; i++)); do
		logs=$($CT logs "$CNAME" 2>&1 | tail -n +$((${1:-0} + 1)))
		if grep -q '"msg":"upstream started","upstream":"npm","tools":[1-9]' <<<"$logs" &&
			grep -q '"msg":"upstream started","upstream":"pip","tools":[1-9]' <<<"$logs" &&
			grep -q '"msg":"upstream started","upstream":"bin","tools":[1-9]' <<<"$logs"; then
			return 0
		fi
		if [ "$($CT inspect -f '{{.State.Running}}' "$CNAME" 2>/dev/null)" != true ]; then
			break
		fi
		sleep 1
	done
	echo "$logs"
	return 1
}

if $CT run -d --name "$CNAME" \
	--read-only --cap-drop=ALL --security-opt no-new-privileges --user 65532:65532 --tmpfs /tmp \
	"${USERNS[@]}" \
	-e GITHUB_CLIENT_SECRET=x \
	-v "$DATA:/data" -v "$DATA/config.yaml:/etc/mcp-dock/config.yaml:ro" -v "$FAKE:/opt/fake:ro" \
	-p 127.0.0.1::8080 "$IMAGE" >/dev/null 2>&1; then
	:
else
	fail three_upstreams "container failed to start"
	exit 1
fi
if logs=$(wait_started); then
	ok three_upstreams
else
	fail three_upstreams "timeout after 300s, logs:"
	echo "$logs"
fi

# 8. healthz
hostport=$($CT port "$CNAME" 8080/tcp 2>/dev/null | head -n1)
hostport=${hostport/0.0.0.0/127.0.0.1}
out=$(curl -s --max-time 10 "http://$hostport/healthz" 2>&1)
if [ "$out" = ok ]; then ok healthz; else fail healthz "got '$out' from $hostport"; fi

# 9. cache_persists
snap() {
	find "$DATA/home/.npm/_cacache/content-v2" -type f 2>/dev/null | sort
	find "$DATA/home/.cache/uv/archive-v0" -mindepth 1 -maxdepth 1 2>/dev/null | sort
}
before=$(snap)
skip=$($CT logs "$CNAME" 2>&1 | wc -l)
$CT stop -t 20 "$CNAME" >/dev/null 2>&1
$CT start "$CNAME" >/dev/null 2>&1
# logs accumulate across restarts, so skip the lines from the first run.
if logs=$(wait_started "$skip"); then
	after=$(snap)
	if [ -n "$before" ] && [ "$before" = "$after" ]; then
		ok cache_persists
	else
		fail cache_persists "listings differ or empty"
		diff <(echo "$before") <(echo "$after") | head -n 20
	fi
else
	fail cache_persists "upstreams didn't come back, logs:"
	echo "$logs"
fi

# 10. no_python_download
if [ ! -e "$DATA/home/.local/share/uv/python" ]; then ok no_python_download; else fail no_python_download "uv downloaded a python"; fi

exit $FAILED
