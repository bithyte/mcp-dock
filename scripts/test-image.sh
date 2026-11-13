#!/usr/bin/env bash
# Smoke test for the mcp-dock container image.
# Usage: scripts/test-image.sh <image-ref>
# Env: CT (container CLI, default podman), EXPECT_VERSION (optional).
set -u

IMAGE=${1:?usage: test-image.sh <image-ref>}
CT=${CT:-podman}
EXPECT_VERSION=${EXPECT_VERSION:-}

FAILED=0
CNAME="mcp-dock-test-$$"
XNAME="mcp-dock-test-export-$$"
VOL="mcp-dock-test-data-$$"
WORK=$(mktemp -d)
CONF="$WORK/conf"
mkdir -p "$CONF"
chmod 0755 "$CONF"

# shellcheck disable=SC2317
cleanup() {
	$CT rm -f "$CNAME" "$XNAME" >/dev/null 2>&1
	$CT volume rm -f "$VOL" >/dev/null 2>&1
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
# The base has no shell or id, so read the image config.
out=$($CT image inspect -f '{{.Config.User}}' "$IMAGE" 2>&1)
if [ "$out" = 65532:65532 ] || [ "$out" = 65532 ]; then ok uid; else fail uid "got '$out'"; fi

# 3. version
if [ -n "$EXPECT_VERSION" ]; then
	out=$($CT run --rm --network none --entrypoint mcp-dock "$IMAGE" version 2>&1)
	if [ "$out" = "$EXPECT_VERSION" ]; then ok version; else fail version "got '$out', want '$EXPECT_VERSION'"; fi
fi

# 4. no_toolchain
# No shell to ask, so list the exported filesystem and look in each PATH dir.
path=$($CT image inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$IMAGE" 2>/dev/null | sed -n 's/^PATH=//p')
listing=""
if [ -n "$path" ] && $CT create --name "$XNAME" "$IMAGE" >/dev/null 2>&1; then
	listing=$($CT export "$XNAME" 2>/dev/null | tar --numeric-owner -tv 2>/dev/null)
fi
# Last field of `tar -tv` is the name (and "-> target" for links, which the
# grep -x below then doesn't match; none of the tools are expected anyway).
files=$(awk '{print $6}' <<<"$listing" | sed 's|^\./||; s|^/||; s|/$||')
if [ -z "$files" ]; then
	fail no_toolchain "couldn't list the image filesystem (PATH='$path')"
elif ! grep -qx 'usr/local/bin/mcp-dock' <<<"$files"; then
	fail no_toolchain "listing has no usr/local/bin/mcp-dock, so it can't be trusted"
else
	found=""
	IFS=: read -ra dirs <<<"$path"
	for d in "${dirs[@]}"; do
		d=${d#/}
		for c in node npx uvx; do
			if grep -qx "$d/$c" <<<"$files"; then found="$found /$d/$c"; fi
		done
	done
	if [ -z "$found" ]; then ok no_toolchain; else fail no_toolchain "found:$found"; fi
fi

# 5. data_dir
# /data must ship owned by 65532 with mode 0700 (checked from the same listing).
line=$(awk '$6 == "data/" || $6 == "./data/" || $6 == "data"' <<<"$listing")
case "$line" in
"drwx------ 65532/65532 "*) ok data_dir ;;
"") fail data_dir "no /data in the image" ;;
*) fail data_dir "got '$line'" ;;
esac

# 6. healthz
# A config with no upstreams; /data is a named volume, which picks up the
# image's 65532-owned /data on first mount.
cat >"$CONF/config.yaml" <<YAML
public_url: http://localhost:8080
listen: ":8080"
data_dir: /data
providers:
  github:
    client_id: test
    client_secret_env: GITHUB_CLIENT_SECRET
allowed_users:
  github: [test]
YAML
chmod 0644 "$CONF/config.yaml"

if $CT run -d --name "$CNAME" \
	--read-only --cap-drop=ALL --security-opt no-new-privileges --user 65532:65532 --tmpfs /tmp \
	-e GITHUB_CLIENT_SECRET=x \
	-v "$VOL:/data" -v "$CONF/config.yaml:/etc/mcp-dock/config.yaml:ro" \
	-p 127.0.0.1::8080 "$IMAGE" >/dev/null 2>&1; then
	hostport=$($CT port "$CNAME" 8080/tcp 2>/dev/null | head -n1)
	hostport=${hostport/0.0.0.0/127.0.0.1}
	out=""
	for ((i = 0; i < 30; i++)); do
		out=$(curl -s --max-time 5 "http://$hostport/healthz" 2>&1)
		[ "$out" = ok ] && break
		if [ "$($CT inspect -f '{{.State.Running}}' "$CNAME" 2>/dev/null)" != true ]; then break; fi
		sleep 1
	done
	if [ "$out" = ok ]; then
		ok healthz
	else
		fail healthz "got '$out' from $hostport, logs:"
		$CT logs "$CNAME" 2>&1 | tail -n 20
	fi
else
	fail healthz "container failed to start"
fi

exit $FAILED
