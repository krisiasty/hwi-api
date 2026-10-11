#!/bin/sh
# shellcheck disable=SC2012 # the helper's directory listings are compared as fixed names, not parsed
# packaging/smoke_test.sh installs a built hwi-api package, starts it
# under the shipped systemd unit, and checks the agent actually works: not
# just that it starts, but that every subsystem returns populated data.
#
# This exists because hardening regressions are silent. A systemd directive
# that costs the agent a hardware read does not produce an error - it
# produces an empty field, a successful request and a 200. Assertion 6
# below is the load-bearing one: if the privileged helper stopped handing
# the root-only DMI files to the daemon, .data.product.data.uuid and
# .data.product.data.serial_number would read back as "unknown" and
# nothing else in this suite, or in the API's own error contract, would
# notice. It also checks the other half of that design: the daemon itself
# runs with no capabilities at all.
#
# Usage: packaging/smoke_test.sh /path/to/hwi-api_*.deb-or-.rpm
#
# Must run as root: installing a package, managing systemd units and
# writing /etc/hwi-api all require it. Exits non-zero on the first
# assertion (numbered, matching the task brief) that does not hold, with a
# one-line message naming which one failed.
set -eu

PKG_NAME=hwi-api
CONFIG_FILE=/etc/hwi-api/config.yaml
TLS_DIR=/etc/hwi-api/tls
API_URL=https://127.0.0.1:8096
OPS_URL=http://127.0.0.1:9096
ACTIVE_TIMEOUT=30
READYZ_TIMEOUT=60

log() {
    printf '[smoke] %s\n' "$*" >&2
}

# diagnose dumps everything needed to tell "the unit is still waiting on a
# dependency" from "the agent is crash-looping" apart, without a second
# round trip through CI to find out. Every command is best-effort: under
# set -e, a failure in here (no journal, no jobs queued, the unit not
# installed yet when a very early setup_fail fires) must not itself abort
# the script before the real failure message is printed, so each one is
# followed by `|| true`.
diagnose() {
    printf '[smoke] ---- diagnostics: systemctl status %s ----\n' "$PKG_NAME" >&2
    systemctl status "$PKG_NAME" --no-pager --full >&2 2>&1 || true
    printf '[smoke] ---- diagnostics: journalctl -u %s (last 100 lines) ----\n' "$PKG_NAME" >&2
    journalctl -u "$PKG_NAME" --no-pager -n 100 >&2 2>&1 || true
    # Distinguishes the two most likely causes of a stuck 'activating':
    # a job queued here and never completing (a dependency, such as
    # network-online.target, that this environment never satisfies) points
    # one way; no job here at all, with the unit merely restarting on a
    # timer, points to the agent crash-looping instead.
    printf '[smoke] ---- diagnostics: systemctl list-jobs ----\n' >&2
    systemctl list-jobs --no-pager >&2 2>&1 || true

    # Credential-store section. This unit's LoadCredential= is how it
    # reaches its TLS material (see packaging/hwi-api.service), and a
    # container is exactly the environment where systemd's per-unit
    # credential store - a private mount systemd itself sets up before the
    # service ever runs - is most likely to behave differently from a real
    # host. $CREDENTIALS_DIRECTORY being set in the agent's own error (it
    # names the path it tried to stat) only proves systemd told the service
    # where to look, not that systemd actually populated it - this section
    # is what tells those two apart.
    printf '[smoke] ---- diagnostics: systemctl --version ----\n' >&2
    systemctl --version 2>&1 | head -1 >&2 || true
    printf '[smoke] ---- diagnostics: ls -la /run/credentials/ ----\n' >&2
    ls -la /run/credentials/ >&2 2>&1 || true
    printf '[smoke] ---- diagnostics: ls -la /run/credentials/%s.service/ ----\n' "$PKG_NAME" >&2
    ls -la "/run/credentials/${PKG_NAME}.service/" >&2 2>&1 || true
    printf '[smoke] ---- diagnostics: systemctl show %s -p LoadCredential -p ExecMainStatus -p User ----\n' "$PKG_NAME" >&2
    systemctl show "$PKG_NAME" -p LoadCredential -p ExecMainStatus -p User >&2 2>&1 || true
    printf '[smoke] ---- diagnostics: journalctl -b | grep -iE credential|namespace|mount (last 40) ----\n' >&2
    journalctl -b --no-pager 2>/dev/null | grep -iE 'credential|namespace|mount' | tail -40 >&2 || true
}

fail() {
    n=$1
    shift
    printf '[smoke] ASSERTION %s FAILED: %s\n' "$n" "$*" >&2
    diagnose
    exit 1
}

setup_fail() {
    printf '[smoke] SETUP FAILED: %s\n' "$*" >&2
    diagnose
    exit 1
}

if [ "$(id -u)" -ne 0 ]; then
    setup_fail "must run as root (installs a package and manages systemd)"
fi

if [ $# -ne 1 ]; then
    setup_fail "usage: $0 /path/to/package.deb-or-.rpm"
fi
PKG=$1
[ -f "$PKG" ] || setup_fail "package not found: $PKG"

for tool in curl jq openssl systemctl sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || setup_fail "required tool not found: $tool"
done

case "$PKG" in
*.deb)
    PKG_FORMAT=deb
    command -v apt-get >/dev/null 2>&1 || setup_fail "apt-get not found for a .deb package"
    ;;
*.rpm)
    PKG_FORMAT=rpm
    if command -v dnf >/dev/null 2>&1; then
        RPM_INSTALLER=dnf
    elif command -v yum >/dev/null 2>&1; then
        RPM_INSTALLER=yum
    else
        setup_fail "neither dnf nor yum found for an .rpm package"
    fi
    ;;
*)
    setup_fail "package path does not end in .deb or .rpm: $PKG"
    ;;
esac

# -p /var/tmp, not the default /tmp: systemd-tmpfiles-setup.service runs
# during boot and wipes /tmp on every boot. The CI job now waits for boot to
# actually finish (systemctl is-system-running --wait - see
# .github/workflows/ci.yml) rather than merely for systemd's manager to
# answer over D-Bus, but a WORKDIR under /tmp would still be one dependency
# away from vanishing out from under this script - the exact race that once
# made this test flaky. /var/tmp is not one of systemd-tmpfiles' boot-time
# cleanup targets, so it survives for the life of this script regardless.
# Do not change this back to a bare `mktemp -d`.
WORKDIR=$(mktemp -d -p /var/tmp)
# Assertion 13's runtime drop-in and its disk-backed mount under /home, and
# assertion 14's user-namespace limit. Each exists only while its assertion
# runs; cleanup puts things back should one fail or be interrupted part-way.
BEFORE_DROPIN_DIR=/run/systemd/system/$PKG_NAME.service.d
BEFORE_DROPIN=$BEFORE_DROPIN_DIR/zz-smoke-before.conf
BEFORE_DROPIN_WRITTEN=0
HOME_FIXTURE=/home/hwi-api-smoke-disk
USERNS_SYSCTL=/proc/sys/user/max_user_namespaces
USERNS_SAVED=""
cleanup() {
    if [ -n "$USERNS_SAVED" ]; then
        echo "$USERNS_SAVED" >"$USERNS_SYSCTL" || true
    fi
    if [ "$BEFORE_DROPIN_WRITTEN" = 1 ]; then
        rm -f "$BEFORE_DROPIN"
        rmdir "$BEFORE_DROPIN_DIR" 2>/dev/null || true
        systemctl daemon-reload || true
    fi
    if mountpoint -q "$HOME_FIXTURE" 2>/dev/null; then
        umount "$HOME_FIXTURE" || true
    fi
    rmdir "$HOME_FIXTURE" 2>/dev/null || true
    rm -rf "$WORKDIR"
}
trap cleanup EXIT
# dash runs the EXIT trap on exit, not when a signal kills it: turn INT
# and TERM into an exit so cleanup runs for them too.
trap 'exit 130' INT
trap 'exit 143' TERM

install_pkg() {
    if [ "$PKG_FORMAT" = deb ]; then
        DEBIAN_FRONTEND=noninteractive apt-get install -y "$1" >"$WORKDIR/install.log" 2>&1 \
            || { cat "$WORKDIR/install.log" >&2; setup_fail "apt-get install failed, see above"; }
    else
        "$RPM_INSTALLER" install -y "$1" >"$WORKDIR/install.log" 2>&1 \
            || { cat "$WORKDIR/install.log" >&2; setup_fail "$RPM_INSTALLER install failed, see above"; }
    fi
}

reinstall_pkg() {
    if [ "$PKG_FORMAT" = deb ]; then
        DEBIAN_FRONTEND=noninteractive apt-get install --reinstall -y "$1" >"$WORKDIR/reinstall.log" 2>&1 \
            || { cat "$WORKDIR/reinstall.log" >&2; setup_fail "apt-get reinstall failed, see above"; }
    else
        "$RPM_INSTALLER" reinstall -y "$1" >"$WORKDIR/reinstall.log" 2>&1 \
            || { cat "$WORKDIR/reinstall.log" >&2; setup_fail "$RPM_INSTALLER reinstall failed, see above"; }
    fi
}

remove_pkg() {
    if [ "$PKG_FORMAT" = deb ]; then
        DEBIAN_FRONTEND=noninteractive apt-get remove -y "$PKG_NAME" >"$WORKDIR/remove.log" 2>&1 \
            || { cat "$WORKDIR/remove.log" >&2; setup_fail "apt-get remove failed, see above"; }
    else
        "$RPM_INSTALLER" remove -y "$PKG_NAME" >"$WORKDIR/remove.log" 2>&1 \
            || { cat "$WORKDIR/remove.log" >&2; setup_fail "$RPM_INSTALLER remove failed, see above"; }
    fi
}

# wait_ready polls GET /readyz once a second until it returns 200, for up to
# $READYZ_TIMEOUT seconds. It returns non-zero on a timeout, with the last
# status in $CODE; $i is the number of seconds it waited.
wait_ready() {
    i=0
    while [ "$i" -lt "$READYZ_TIMEOUT" ]; do
        CODE=$(curl -s -o /dev/null -w '%{http_code}' "$OPS_URL/readyz" || echo 000)
        if [ "$CODE" = "200" ]; then
            return 0
        fi
        i=$((i + 1))
        sleep 1
    done
    return 1
}

# fetch_inventory writes GET /api/v1/inventory's body, fetched with the
# test's own CA and bearer token, to the file $1 and prints the status.
fetch_inventory() {
    curl -s --cacert "$TLS_DIR/cert.pem" \
        -H "Authorization: Bearer $TOKEN" \
        -o "$1" -w '%{http_code}' \
        "$API_URL/api/v1/inventory"
}

log "installing $PKG"
install_pkg "$PKG"

# --- Assertion 0: the unit starts with no TLS files installed -----------
# LoadCredential= makes systemd refuse to start a unit whose source is
# missing, before any of its commands run and whatever the agent's own
# configuration says. The unit loads $TLS_DIR as a directory, which the
# package creates empty, so a host running with api.tls.enabled: false
# needs no certificate at all. This runs on the fresh install, before the
# pair below is generated.
TLS_DIR_PERMS=$(stat -c '%a %U:%G' "$TLS_DIR" 2>/dev/null || echo missing)
[ "$TLS_DIR_PERMS" = "700 root:root" ] \
    || fail 0 "$TLS_DIR is '$TLS_DIR_PERMS' after install, want '700 root:root', created by the package"
if [ -n "$(ls -A "$TLS_DIR")" ]; then
    setup_fail "$TLS_DIR is not empty on a fresh install"
fi
log "starting with no TLS files and a configuration with TLS and auth off"
cat >"$CONFIG_FILE" <<EOF
api:
  tls:
    enabled: false
  auth:
    enabled: false
  access:
    allow: ["127.0.0.1/32"]
EOF
chown root:"$PKG_NAME" "$CONFIG_FILE"
chmod 0640 "$CONFIG_FILE"
systemctl daemon-reload
systemctl start "$PKG_NAME" >"$WORKDIR/start-notls.log" 2>&1 \
    || { cat "$WORKDIR/start-notls.log" >&2; journalctl -u "$PKG_NAME" --no-pager -n 100 >&2 || true; fail 0 "$PKG_NAME did not start with no TLS files installed"; }
wait_ready || fail 0 "GET $OPS_URL/readyz did not return 200 within ${READYZ_TIMEOUT}s with no TLS files installed (last status: $CODE)"
systemctl stop "$PKG_NAME"
log "assertion 0 OK: $PKG_NAME started and became ready with no TLS files installed"

log "generating a self-signed certificate for 127.0.0.1 and ::1"
mkdir -p "$TLS_DIR"
chmod 0750 "$TLS_DIR"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -keyout "$TLS_DIR/key.pem" -out "$TLS_DIR/cert.pem" \
    -subj "/CN=hwi-api-smoke-test" \
    -addext "subjectAltName=IP:127.0.0.1,IP:::1,DNS:localhost" \
    >"$WORKDIR/openssl.log" 2>&1 \
    || { cat "$WORKDIR/openssl.log" >&2; setup_fail "openssl certificate generation failed"; }
chmod 0600 "$TLS_DIR/key.pem" "$TLS_DIR/cert.pem"
chown root:root "$TLS_DIR/key.pem" "$TLS_DIR/cert.pem"

log "generating a bearer token and its digest"
TOKEN=$(openssl rand -hex 32)
DIGEST=$(printf '%s' "$TOKEN" | sha256sum | awk '{print $1}')

log "writing a working configuration to $CONFIG_FILE"
cat >"$CONFIG_FILE" <<EOF
api:
  bind:
    addresses: []
    interfaces: []
    port: 8096
  tls:
    cert_file: credential:tls_cert.pem
    key_file: credential:tls_key.pem
    min_version: "1.3"
  auth:
    mtls:
      enabled: false
      allowed_subjects: []
    token:
      enabled: true
      hashes: ["sha256:$DIGEST"]
  access:
    deny: []
    # Deliberately 127.0.0.1/32 only, not ::1: assertion 8 depends on ::1
    # being a real, disallowed source address rather than a simulated one.
    allow: ["127.0.0.1/32"]
  timeouts:
    read_header: 5s
    read: 10s
    write: 30s
    idle: 60s
  max_concurrent_requests: 32
ops:
  bind:
    addresses: ["127.0.0.1", "::1"]
    interfaces: []
    port: 9096
  access:
    allow: ["127.0.0.1/32", "::1/128"]
    deny: []
inventory:
  refresh:
    interval: 1m
log:
  level: info
EOF
chown root:"$PKG_NAME" "$CONFIG_FILE"
chmod 0640 "$CONFIG_FILE"
CONFIG_SUM_BEFORE=$(sha256sum "$CONFIG_FILE" | awk '{print $1}')

log "starting the service"
systemctl daemon-reload
systemctl enable --now "$PKG_NAME" >"$WORKDIR/start.log" 2>&1 \
    || { cat "$WORKDIR/start.log" >&2; journalctl -u "$PKG_NAME" --no-pager -n 100 >&2 || true; setup_fail "systemctl enable --now $PKG_NAME failed"; }

# --- Assertion 1: systemctl is-active hwi-api -> active ---------------
# `systemctl enable --now` returning is not the same event as the unit's
# ActiveState reaching "active": found the hard way in CI, where a plain,
# one-shot `is-active` read "activating" 278ms after enable --now returned.
# The unit is Type=exec, so systemd is content once the binary has been
# exec'd; there is no guarantee that transition is already visible to a
# separate query issued immediately afterwards. So this polls instead of
# sampling once, exactly like assertion 2 below it - but "activating" is
# the only state worth waiting out. "failed" or "inactive" mean the unit
# has already given up and is not going to become active on its own, so
# those fail immediately rather than burning the rest of the timeout.
STATE=""
i=0
while [ "$i" -lt "$ACTIVE_TIMEOUT" ]; do
    STATE=$(systemctl is-active "$PKG_NAME" || true)
    case "$STATE" in
    active)
        break
        ;;
    failed | inactive)
        fail 1 "systemctl is-active $PKG_NAME reported '$STATE', want 'active' (it will not recover on its own; see: journalctl -u $PKG_NAME)"
        ;;
    esac
    i=$((i + 1))
    sleep 1
done
[ "$STATE" = "active" ] \
    || fail 1 "systemctl is-active $PKG_NAME did not reach 'active' within ${ACTIVE_TIMEOUT}s (last state: '$STATE')"
log "assertion 1 OK: $PKG_NAME is active (after ${i}s)"

# --- Assertion 2: GET /readyz -> 200 within 60 seconds -------------------
wait_ready || fail 2 "GET $OPS_URL/readyz did not return 200 within ${READYZ_TIMEOUT}s (last status: $CODE)"
log "assertion 2 OK: /readyz returned 200 after ${i}s"

# --- Assertion 3: GET /status -> name, version, commit populated --------
STATUS_BODY=$(curl -s -w '\n%{http_code}' "$OPS_URL/status")
STATUS_CODE=$(printf '%s' "$STATUS_BODY" | tail -n1)
STATUS_JSON=$(printf '%s' "$STATUS_BODY" | sed '$d')
[ "$STATUS_CODE" = "200" ] || fail 3 "GET $OPS_URL/status returned $STATUS_CODE, want 200"
echo "$STATUS_JSON" | jq -e '
    (.name // "" | length > 0) and
    (.version // "" | length > 0) and
    (.commit // "" | length > 0)
' >/dev/null || fail 3 "GET $OPS_URL/status did not have non-empty name, version and commit: $STATUS_JSON"
log "assertion 3 OK: /status has name, version and commit populated"

# --- Assertion 4: GET /api/v1/inventory -> 200 and .complete == true ----
INV_BODY_FILE="$WORKDIR/inventory.json"
INV_CODE=$(fetch_inventory "$INV_BODY_FILE")
[ "$INV_CODE" = "200" ] || fail 4 "GET $API_URL/api/v1/inventory returned $INV_CODE, want 200 (body: $(cat "$INV_BODY_FILE"))"
jq -e '.complete == true' "$INV_BODY_FILE" >/dev/null \
    || fail 4 ".complete was not true: $(jq -c '.complete, .errors, .disabled' "$INV_BODY_FILE")"
log "assertion 4 OK: /api/v1/inventory returned 200 with complete == true"

# --- Assertion 5: every subsystem in .data is non-empty ------------------
SUBSYSTEMS=$(jq -r '.data | keys[]' "$INV_BODY_FILE")
[ -n "$SUBSYSTEMS" ] || fail 5 ".data had no subsystems at all"
for sub in $SUBSYSTEMS; do
    jq -e --arg sub "$sub" '
        .data[$sub].data as $d
        | ($d != null) and ($d != {})
    ' "$INV_BODY_FILE" >/dev/null \
        || fail 5 "subsystem '$sub' has empty data: $(jq -c --arg sub "$sub" '.data[$sub].data' "$INV_BODY_FILE")"
done
log "assertion 5 OK: every subsystem in .data is non-empty ($(printf '%s\n' "$SUBSYSTEMS" | wc -l | tr -d ' ') subsystems)"

# The network subsystem asks the kernel's ethtool netlink family for each
# interface's features. That has to work under RestrictAddressFamilies= with
# AF_NETLINK, SystemCallFilter=@system-service and no capabilities, and a
# failure there is only an ethtool_error field in an otherwise successful
# response. A container's interface is normally eth0; where it has none, the
# per-interface check is skipped.
NET_ERROR=$(jq -r '.data.network.data.ethtool_error // ""' "$INV_BODY_FILE")
[ -z "$NET_ERROR" ] || fail 5 "network reports a top-level ethtool_error: $NET_ERROR"
# Each interface's permanent address comes from one rtnetlink link dump,
# which has to work under the same sandbox.
PERM_ERROR=$(jq -r '.data.network.data.permanent_mac_error // ""' "$INV_BODY_FILE")
[ -z "$PERM_ERROR" ] || fail 5 "network reports a permanent_mac_error: $PERM_ERROR"
if jq -e '.data.network.data.nics // [] | any(.name == "eth0")' "$INV_BODY_FILE" >/dev/null; then
    ETH0_ERROR=$(jq -r '.data.network.data.nics[] | select(.name == "eth0") | .ethtool_error // ""' "$INV_BODY_FILE")
    [ -z "$ETH0_ERROR" ] || fail 5 "network nic eth0 has an ethtool_error: $ETH0_ERROR"
    ETH0_FEATURES=$(jq -r '.data.network.data.nics[] | select(.name == "eth0") | (.features // {}) | length' "$INV_BODY_FILE")
    [ "$ETH0_FEATURES" -gt 0 ] 2>/dev/null \
        || fail 5 "network nic eth0 reports no ethtool features although it has no ethtool_error"
    log "network nic eth0 reports $ETH0_FEATURES ethtool features over netlink"
else
    log "no eth0 in the network payload; ethtool features not checked"
fi

# udev's database is read as the daemon's own user. Where the host has one
# with block devices in it (a container without udevd has none, and then
# the udev-* subsystems rightly report no devices), udev-block must report
# them, resolved through sysfs to a /dev node.
if ls /run/udev/data/b* >/dev/null 2>&1; then
    UDEV_BLOCK=$(jq -r '[.data["udev-block"].data.devices[] | select(.devname | startswith("/dev/"))] | length' "$INV_BODY_FILE")
    [ "$UDEV_BLOCK" -gt 0 ] 2>/dev/null \
        || fail 5 "udev-block reports no block device with a /dev node although /run/udev/data has block entries"
    log "udev-block reports $UDEV_BLOCK block devices from udev's database"
else
    log "no block devices in udev's database here; udev-block not checked"
fi

# block lists /sys/block itself, which a container shares with its host.
# Its refresh must not fail, and every device it would report (all but an
# unused, size-0 loop, nbd, ram or zram device and a hidden NVMe multipath
# path, as skipBlockDevice skips them) must give it at least one device, so
# a probe that silently lists nothing is caught.
jq -e '.data | has("block")' "$INV_BODY_FILE" >/dev/null \
    || fail 5 "the inventory has no block subsystem: $(jq -c '.errors' "$INV_BODY_FILE")"
BLOCK_ERROR=$(jq -r '.errors.block // ""' "$INV_BODY_FILE")
[ -z "$BLOCK_ERROR" ] || fail 5 "block reports an error: $BLOCK_ERROR"
SYS_BLOCK=0
for dev in /sys/block/*; do
    [ -e "$dev" ] || continue
    [ "$(cat "$dev/hidden" 2>/dev/null)" = "1" ] && continue
    case "${dev##*/}" in
        loop*|nbd*|ram*|zram*) [ "$(cat "$dev/size" 2>/dev/null)" = "0" ] && continue ;;
    esac
    SYS_BLOCK=$((SYS_BLOCK + 1))
done
BLOCK_DEVICES=$(jq -r '.data.block.data.devices | length' "$INV_BODY_FILE")
if [ "$SYS_BLOCK" -gt 0 ]; then
    [ "$BLOCK_DEVICES" -gt 0 ] 2>/dev/null \
        || fail 5 "block reports no devices although /sys/block has $SYS_BLOCK it should list"
    log "block reports $BLOCK_DEVICES devices for $SYS_BLOCK in /sys/block"
else
    log "no block devices to list in /sys/block here; block device count not checked"
fi

# cpu is read from /sys/devices/system/cpu and /proc/cpuinfo, which a
# container shares with its host. Its refresh must not fail, and it must
# count every online CPU, the ones /proc/cpuinfo has a processor line for,
# so a probe that loses CPUs to an unreadable topology is caught. The
# equality assumes /proc/cpuinfo is not virtualised (lxcfs shows a
# container only the CPUs its limits allow).
jq -e '.data | has("cpu")' "$INV_BODY_FILE" >/dev/null \
    || fail 5 "the inventory has no cpu subsystem: $(jq -c '.errors' "$INV_BODY_FILE")"
CPU_ERROR=$(jq -r '.errors.cpu // ""' "$INV_BODY_FILE")
[ -z "$CPU_ERROR" ] || fail 5 "cpu reports an error: $CPU_ERROR"
ONLINE_CPUS=$(grep -c '^processor' /proc/cpuinfo)
CPU_THREADS=$(jq -r '.data.cpu.data.total_hardware_threads' "$INV_BODY_FILE")
[ "$CPU_THREADS" = "$ONLINE_CPUS" ] \
    || fail 5 "cpu reports $CPU_THREADS hardware threads, but /proc/cpuinfo lists $ONLINE_CPUS online CPUs"
log "cpu reports $CPU_THREADS hardware threads in $(jq -r '.data.cpu.data.processors | length' "$INV_BODY_FILE") processors"

# topology groups the same online CPUs by NUMA node, so its nodes' cores
# must hold as many logical CPUs as cpu counts: a CPU missing from every
# node, or listed in two, is caught.
jq -e '.data | has("topology")' "$INV_BODY_FILE" >/dev/null \
    || fail 5 "the inventory has no topology subsystem: $(jq -c '.errors' "$INV_BODY_FILE")"
TOPOLOGY_ERROR=$(jq -r '.errors.topology // ""' "$INV_BODY_FILE")
[ -z "$TOPOLOGY_ERROR" ] || fail 5 "topology reports an error: $TOPOLOGY_ERROR"
TOPOLOGY_THREADS=$(jq -r '[.data.topology.data.nodes[].cores[].logical_processors[]] | length' "$INV_BODY_FILE")
[ "$TOPOLOGY_THREADS" = "$CPU_THREADS" ] \
    || fail 5 "topology's nodes hold $TOPOLOGY_THREADS logical processors, but cpu reports $CPU_THREADS hardware threads"
log "topology reports $TOPOLOGY_THREADS logical processors in $(jq -r '.data.topology.data.nodes | length' "$INV_BODY_FILE") nodes"

# --- Assertion 6: product.data.uuid and .serial_number are non-empty,
# --- and the daemon holds no capabilities -------------------------------
# This is the hardening assertion. /sys/class/dmi/id/product_uuid and
# product_serial are mode 0400, root-only, and the daemon runs as the
# hwi-api user with no capabilities, so it cannot read them itself. The
# privileged helper reads them on the daemon's behalf, over a socket, and
# the product prober reports what it returns. If that broke, the request
# would still be a 200, and nothing else in this script would fail.
#
# Without the helper the agent reports these fields as the literal
# "unknown", and a helper that does not answer
# fails the probe, so a fresh daemon has no product payload at all. A
# check for a merely non-empty value would pass right through that
# sentinel and miss the exact regression this assertion exists to catch,
# so both forms of "no real data" are rejected here.
PRODUCT_UUID=$(jq -r '.data.product.data.uuid // ""' "$INV_BODY_FILE")
PRODUCT_SERIAL=$(jq -r '.data.product.data.serial_number // ""' "$INV_BODY_FILE")
case "$PRODUCT_UUID" in
"" | unknown) fail 6 ".data.product.data.uuid is '$PRODUCT_UUID' -- DMI identity fields were not handed over; check journalctl -u 'hwi-api-helper@*' and journalctl -u $PKG_NAME" ;;
esac
case "$PRODUCT_SERIAL" in
"" | unknown) fail 6 ".data.product.data.serial_number is '$PRODUCT_SERIAL' -- DMI identity fields were not handed over; check journalctl -u 'hwi-api-helper@*' and journalctl -u $PKG_NAME" ;;
esac

# /run must be root's alone, so that only root can create or replace the
# helper's socket in it.
PARENT_PERMS=$(stat -c '%a %U:%G' /run 2>/dev/null || echo missing)
# 755 only: a world-writable /run, even with the sticky bit, would let the
# daemon's user pre-create the socket path before systemd does.
case "$PARENT_PERMS" in
755\ root:*) ;;
*) fail 6 "/run is '$PARENT_PERMS', want '755 root:...', so that only root can create entries in it" ;;
esac
# The SMBIOS table is read through the helper too, for the memory modules.
# A host without one (some VMs) legitimately reports none, so this checks
# the modules only where the host has a table to read.
if [ -e /sys/firmware/dmi/tables/DMI ]; then
    MODULES_TYPE=$(jq -r '.data.memory.data.modules | type' "$INV_BODY_FILE")
    MODULES_ERROR=$(jq -r '.data.memory.data.modules_error // ""' "$INV_BODY_FILE")
    if [ "$MODULES_TYPE" != array ] || [ -n "$MODULES_ERROR" ]; then
        fail 6 ".data.memory.data.modules is a $MODULES_TYPE ('$MODULES_ERROR') -- the SMBIOS table was not handed over or not decoded; check journalctl -u 'hwi-api-helper@*' and journalctl -u $PKG_NAME"
    fi
    log "memory modules read from the SMBIOS table: $(jq '.data.memory.data.modules | length' "$INV_BODY_FILE") slots"
    # The expansion slots come from the same table. A host whose firmware
    # describes none reports an empty list, not an error.
    SLOTS_ERROR=$(jq -r '.data.slots.data.slots_error // ""' "$INV_BODY_FILE")
    case "$SLOTS_ERROR" in
    *unavailable* | *"not configured"*)
        fail 6 ".data.slots.data.slots_error is '$SLOTS_ERROR' -- the SMBIOS table was not handed over; check journalctl -u 'hwi-api-helper@*' and journalctl -u $PKG_NAME"
        ;;
    esac
else
    log "host has no SMBIOS table; memory modules and slots not checked"
fi

# Each PCI device's Vital Product Data is read through the helper as well.
# A powered-down card, or one whose VPD cannot be read or decoded, rightly
# reports a vpd_error; a helper the daemon was not given, or cannot reach,
# must not be the reason. Only counts and errors are logged: VPD holds
# serial numbers.
VPD_DEVICES=0
VPD_READ=0
for vpd in /sys/bus/pci/devices/*/vpd; do
    [ -e "$vpd" ] || continue
    addr=$(basename "$(dirname "$vpd")")
    VPD_DEVICES=$((VPD_DEVICES + 1))
    VPD_STATE=$(jq -r --arg addr "$addr" '
        [(.data.pci.data.devices // [])[] | select(.address == $addr)] as $d
        | if ($d | length) == 0 then "missing"
          elif $d[0].vpd != null then "vpd"
          elif ($d[0].vpd_error // "") == "" then "neither vpd nor vpd_error"
          else "error: " + $d[0].vpd_error end
    ' "$INV_BODY_FILE")
    case "$VPD_STATE" in
    vpd) VPD_READ=$((VPD_READ + 1)) ;;
    "error: "*"not configured"* | "error: "*unavailable*)
        fail 6 "PCI device $addr has vpd_error '${VPD_STATE#error: }' -- the helper did not read its VPD; check journalctl -u 'hwi-api-helper@*' and journalctl -u $PKG_NAME"
        ;;
    "error: "*) log "PCI device $addr: ${VPD_STATE}" ;;
    *) fail 6 "PCI device $addr has a vpd file, but .data.pci.data.devices reports it as: $VPD_STATE" ;;
    esac
done
if [ "$VPD_DEVICES" -gt 0 ]; then
    log "PCI VPD read through the helper for $VPD_READ of $VPD_DEVICES devices with a vpd file (values withheld)"
else
    log "no PCI device has VPD; VPD not checked"
fi

# The socket: the daemon's group may connect, nobody else may, and only
# root can replace it, since /run is root's alone (checked above).
SOCK=/run/hwi-api-helper.sock
SOCK_PERMS=$(stat -c '%a %U:%G %F' "$SOCK" 2>/dev/null || echo missing)
[ "$SOCK_PERMS" = "660 root:$PKG_NAME socket" ] \
    || fail 6 "$SOCK is '$SOCK_PERMS', want '660 root:$PKG_NAME socket'"
if runuser -u "$PKG_NAME" -- test -w /run; then
    fail 6 "/run is writable by the $PKG_NAME user"
fi
[ ! -e /run/hwi-api-dmi ] || fail 6 "/run/hwi-api-dmi exists; the startup copies it held are gone"

# A deny-listed file system must be mounted under the host's /sys for the
# sysfs-submount check below to catch a recursive bind. CI containers often
# have none, so try to mount tracefs, and say what is there either way.
if [ -d /sys/kernel/tracing ] && ! mountpoint -q /sys/kernel/tracing; then
    mount -t tracefs tracefs /sys/kernel/tracing 2>/dev/null || true
fi
HOST_DENY=$(awk '{ n = split($0, parts, " - "); split(parts[n], f, " ");
    if (f[1] ~ /^(debugfs|tracefs|securityfs|efivarfs|pstore|bpf|configfs|fusectl)$/) { print f[1] " at " $5 } }' /proc/1/mountinfo)
if [ -n "$HOST_DENY" ]; then
    echo "$HOST_DENY" | while IFS= read -r line; do log "host mounts $line; the helper must not see it"; done
else
    log "no deny-listed filesystem is mounted on the host; the sysfs-submount check cannot catch a recursive bind here"
fi

# A live helper instance: hold one connection open as the daemon's user,
# with socat as the connecting process, and pick the instance systemd
# started for that connection. Accept=yes instance names end in
# <connection number>-<peer pid>-<peer uid>, so the peer pid finds it.
runuser -u "$PKG_NAME" -- socat -u EXEC:'sleep 20' UNIX-CONNECT:"$SOCK" &
HOLDER=$!
SOCAT_PID=0
HELPER_PID=0
HELPER_WANT_UID=$(id -u hwi-api-helper)
i=0
while [ "$i" -lt 10 ]; do
    SOCAT_PID=$(pgrep -u "$PKG_NAME" -x socat | head -n 1 || true)
    if [ -n "$SOCAT_PID" ]; then
        UNIT=$(systemctl list-units --plain --no-legend --state=running 'hwi-api-helper@*.service' \
            | awk -v pid="$SOCAT_PID" '$1 ~ ("-" pid "-[0-9]+\\.service$") { print $1; exit }')
        if [ -n "$UNIT" ]; then
            HELPER_PID=$(systemctl show -p MainPID --value "$UNIT")
            # MainPID is set as soon as systemd forks the child, which is
            # still root while it sets up the namespace and credentials. Only
            # accept the process once it has exec'd the helper: the right
            # binary and the unprivileged user. Otherwise the checks below
            # would inspect the pre-exec process.
            if [ "$HELPER_PID" != 0 ]; then
                HELPER_EXE=$(readlink "/proc/$HELPER_PID/exe" 2>/dev/null || true)
                HELPER_UID=$(awk '$1 == "Uid:" { print $2 }' "/proc/$HELPER_PID/status" 2>/dev/null || true)
                if [ "$HELPER_EXE" = /usr/bin/hwi-api ] && [ "$HELPER_UID" = "$HELPER_WANT_UID" ]; then
                    break
                fi
                HELPER_PID=0
            fi
        fi
    fi
    i=$((i + 1))
    sleep 1
done
[ "$HELPER_PID" != 0 ] || fail 6 "no hwi-api-helper@ instance had exec'd hwi-api as hwi-api-helper for a connection from $PKG_NAME"
# CAP_DAC_READ_SEARCH (bit 2) and nothing else, in every set.
for set in CapPrm CapEff CapAmb CapBnd; do
    VALUE=$(awk -v k="$set:" '$1 == k { print $2 }' "/proc/$HELPER_PID/status")
    [ "$VALUE" = "0000000000000004" ] \
        || fail 6 "the helper (pid $HELPER_PID) has $set=$VALUE, want CAP_DAC_READ_SEARCH alone (0000000000000004)"
done
# Its file system, as an allowlist. systemd adds its API filesystems and
# /run to the empty root, so besides /sys, its binary and /etc/passwd the
# instance has a /proc showing only itself (ProtectProc=invisible,
# ProcSubset=pid), a minimal private /dev and a /run with no files or
# sockets in it.
[ -e "/proc/$HELPER_PID/root/sys/class/dmi/id" ] || fail 6 "the helper cannot see /sys"
HELPER_ROOT="/proc/$HELPER_PID/root"
SEEN=$(ls -A "$HELPER_ROOT" | tr '\n' ' ')
[ "$SEEN" = "dev etc proc run sys usr " ] || fail 6 "the helper's / holds '$SEEN', want 'dev etc proc run sys usr '"
# /proc: this test runs as root, which hidepid does not filter, so other
# processes' directories are visible here whatever the confinement. What
# can be checked from outside is that nothing but PID directories, self and
# thread-self is shown (subset=pid hides the rest for every reader), and
# that the mount carries hidepid=invisible and subset=pid.
OTHERS=""
for path in "$HELPER_ROOT"/proc/*; do
    entry=${path##*/}
    case "$entry" in
        self | thread-self) ;;
        *[!0-9]*) OTHERS="$OTHERS $entry" ;;
        *) ;;
    esac
done
[ -z "$OTHERS" ] || fail 6 "the helper's /proc shows non-PID entries:$(echo "$OTHERS" | tr ' ' '\n' | head -n 5 | tr '\n' ' ')"
PROC_OPTS=$(awk '$5 == "/proc" { n = split($0, parts, " - "); split(parts[n], f, " "); print f[3] }' "/proc/$HELPER_PID/mountinfo")
case ",$PROC_OPTS," in
    *,hidepid=invisible,* | *,hidepid=4,*) ;;
    *) fail 6 "the helper's /proc mount options are '$PROC_OPTS', want hidepid=invisible" ;;
esac
case ",$PROC_OPTS," in
    *,subset=pid,*) ;;
    *) fail 6 "the helper's /proc mount options are '$PROC_OPTS', want subset=pid" ;;
esac
# /sys: bound without its submounts. Log what systemd mounts below it
# (systemd's own protections may appear under /sys as read-only sysfs
# self-binds or tmpfs stubs, e.g. for /sys/fs/cgroup and /sys/fs/bpf; those
# are expected; so is the cgroup tree under /sys/fs/cgroup, cgroup2 or cgroup
# v1 controllers on tmpfs, which systemd needs), and fail on the file systems that would hold root-only files
# the helper's capability could read.
awk '$5 ~ "^/sys" { n = split($0, parts, " - "); split(parts[n], f, " "); print $5 " " f[1] }' "/proc/$HELPER_PID/mountinfo" \
    | while IFS= read -r line; do log "helper mount under /sys: $line"; done
BAD_MOUNT=$(awk '{ n = split($0, parts, " - "); split(parts[n], f, " ");
    if (f[1] ~ /^(debugfs|tracefs|securityfs|efivarfs|pstore|bpf|configfs|fusectl)$/) { print f[1] " at " $5; exit } }' "/proc/$HELPER_PID/mountinfo")
[ -z "$BAD_MOUNT" ] || fail 6 "the helper sees a sysfs submount: $BAD_MOUNT"
# /dev: no block devices, no memory devices.
BLOCKS=$(find "$HELPER_ROOT/dev" -type b | head -n 5 | tr '\n' ' ')
[ -z "$BLOCKS" ] || fail 6 "the helper's /dev holds block devices: $BLOCKS"
for dev in mem kmem port; do
    [ ! -e "$HELPER_ROOT/dev/$dev" ] || fail 6 "the helper's /dev holds $dev"
done
# /run: log it for the record; directories are fine, files and sockets not.
find "$HELPER_ROOT/run" | head -n 50 | while IFS= read -r line; do log "helper /run: $line"; done
RUN_FILES=$(find "$HELPER_ROOT/run" \( -type f -o -type s \) | head -n 5 | tr '\n' ' ')
[ -z "$RUN_FILES" ] || fail 6 "the helper's /run holds files or sockets: $RUN_FILES"
SEEN=$(ls -A "$HELPER_ROOT/etc" | tr '\n' ' ')
[ "$SEEN" = "passwd " ] || fail 6 "the helper's /etc holds '$SEEN', want 'passwd '"
SEEN=$(ls -A "$HELPER_ROOT/usr" | tr '\n' ' ')
[ "$SEEN" = "bin " ] || fail 6 "the helper's /usr holds '$SEEN', want 'bin '"
SEEN=$(ls -A "$HELPER_ROOT/usr/bin" | tr '\n' ' ')
[ "$SEEN" = "hwi-api " ] || fail 6 "the helper's /usr/bin holds '$SEEN', want 'hwi-api '"
# Its network namespace has loopback only.
IFACES=$(awk -F: 'NR > 2 { gsub(/ /, "", $1); print $1 }' "/proc/$HELPER_PID/net/dev" | tr '\n' ' ')
[ "$IFACES" = "lo " ] || fail 6 "the helper's network namespace has interfaces '$IFACES', want only lo"
kill "$SOCAT_PID" "$HOLDER" 2>/dev/null || true
wait "$HOLDER" 2>/dev/null || true

# The other half of the design: the network-facing process holds no
# capabilities, so a compromise cannot read files beyond what the
# hwi-api user itself can. All three sets must be empty.
MAIN_PID=$(systemctl show -p MainPID --value "$PKG_NAME")
if [ -z "$MAIN_PID" ] || [ "$MAIN_PID" = 0 ]; then
    fail 6 "could not find $PKG_NAME's main PID"
fi
for set in CapPrm CapEff CapAmb; do
    VALUE=$(awk -v k="$set:" '$1 == k { print $2 }' "/proc/$MAIN_PID/status")
    [ "$VALUE" = "0000000000000000" ] \
        || fail 6 "$PKG_NAME (pid $MAIN_PID) has $set=$VALUE, want no capabilities"
done
log "assertion 6 OK: product.uuid and product.serial_number are populated through the helper (values withheld), its socket is root-owned and reachable by $PKG_NAME only, a live helper instance holds CAP_DAC_READ_SEARCH alone and sees only /sys (no sysfs submounts such as debugfs), its binary, /etc/passwd, its own /proc entry and a private /dev, and the daemon holds no capabilities"

# --- Assertion 7: an unauthenticated request -> 401 ----------------------
UNAUTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$TLS_DIR/cert.pem" \
    "$API_URL/api/v1/inventory")
[ "$UNAUTH_CODE" = "401" ] || fail 7 "an unauthenticated request returned $UNAUTH_CODE, want 401"
log "assertion 7 OK: an unauthenticated request returned 401"

# --- Assertion 8: a request from a disallowed source address -> 403 -----
# api.access.allow above is 127.0.0.1/32 only. This host also has ::1, a
# real disallowed source address, so this exercises the deny path for
# real rather than simulating it.
DISALLOWED_CODE=$(curl -s -o /dev/null -w '%{http_code}' -6 --cacert "$TLS_DIR/cert.pem" \
    -H "Authorization: Bearer $TOKEN" \
    "https://[::1]:8096/api/v1/inventory")
[ "$DISALLOWED_CODE" = "403" ] || fail 8 "a request from ::1 (not in api.access.allow) returned $DISALLOWED_CODE, want 403"
log "assertion 8 OK: a request from a disallowed source address (::1) returned 403"

# --- Assertion 12: the unit's sandbox is in effect ----------------------
# Numbered after the others, which keep their numbers; it runs here, while
# the daemon is up under the shipped unit, before assertions 9 to 11 take
# the package away. Each directive is checked on the running process, not
# in the unit file: one that this host's systemd or kernel ignored would
# still be in the file. sandbox_state describes the process; assertion 13
# uses it too, to prove that its "before" run really ran without them.

# find_daemon puts the daemon's main PID in $DAEMON_PID, or fails
# assertion $1.
find_daemon() {
    DAEMON_PID=$(systemctl show -p MainPID --value "$PKG_NAME" || true)
    if [ -z "$DAEMON_PID" ] || [ "$DAEMON_PID" = 0 ]; then
        fail "$1" "could not find $PKG_NAME's main PID"
    fi
}

# sandbox_state prints one directive=value word per directive, in a fixed
# order, as the daemon's process $1 shows it. Most are read from the
# process itself, from outside. Two cannot be read from outside more
# cheaply than from systemd, so they come from systemctl show: which system
# calls the seccomp filter refuses, and how (only that a filter is loaded
# shows in /proc), and RemoveIPC=, which acts only when the unit stops.
# ExecPaths= is read from both. It fails nothing; callers compare its
# output.
sandbox_state() {
    pid=$1
    # PrivateDevices=: a /dev of its own, with no block device in it.
    disks=""
    for path in "/proc/$pid/root/dev/"sd* "/proc/$pid/root/dev/"nvme* "/proc/$pid/root/dev/"vd*; do
        if [ -e "$path" ]; then
            disks="$disks${path##*/},"
        fi
    done
    blocks=$(find "/proc/$pid/root/dev" -type b 2>/dev/null | head -n 3 | sed 's|.*/||' | tr '\n' ',' || true)
    if [ "$(stat -c %d "/proc/$pid/root/dev" 2>/dev/null || true)" = "$(stat -c %d /dev)" ]; then
        state="PrivateDevices=no"
    elif [ -n "$blocks$disks" ]; then
        state="PrivateDevices=private-but-holds:$blocks$disks"
    else
        state="PrivateDevices=yes"
    fi
    # PrivateIPC=: an IPC namespace other than PID 1's.
    ipc=$(readlink "/proc/$pid/ns/ipc" 2>/dev/null || true)
    if [ -z "$ipc" ]; then
        state="$state PrivateIPC=unreadable"
    elif [ "$ipc" = "$(readlink /proc/1/ns/ipc)" ]; then
        state="$state PrivateIPC=no"
    else
        state="$state PrivateIPC=yes"
    fi
    state="$state RemoveIPC=$(systemctl show -p RemoveIPC --value "$PKG_NAME" || true)"
    state="$state UMask=$(awk '$1 == "Umask:" { print $2 }' "/proc/$pid/status" 2>/dev/null || true)"
    # SystemCallFilter=: a seccomp filter is loaded (Seccomp: 2), and the
    # allow list systemd resolved has setrlimit (@resources) and setuid
    # (@privileged) either both in it, @system-service alone, or both out.
    seccomp=$(awk '$1 == "Seccomp:" { print $2 }' "/proc/$pid/status" 2>/dev/null || true)
    filter=" $(systemctl show -p SystemCallFilter --value "$PKG_NAME" || true) "
    if [ "$seccomp" != 2 ]; then
        state="$state SystemCallFilter=not-loaded:$seccomp"
    else
        case "$filter" in
        " ~"*) state="$state SystemCallFilter=deny-list" ;;
        *" openat "*)
            case "$filter" in *" setrlimit "*) rl=in ;; *) rl=out ;; esac
            case "$filter" in *" setuid "*) su=in ;; *) su=out ;; esac
            case "$rl$su" in
            inin) state="$state SystemCallFilter=@system-service" ;;
            outout) state="$state SystemCallFilter=@system-service,~@privileged,~@resources" ;;
            *) state="$state SystemCallFilter=setrlimit-$rl,setuid-$su" ;;
            esac
            ;;
        *) state="$state SystemCallFilter=unrecognised" ;;
        esac
    fi
    # systemd prints the errno by name or by number depending on its
    # version, and the default, killing the process, as "kill" or as
    # SECCOMP_ERROR_NUMBER_KILL.
    errno=$(systemctl show -p SystemCallErrorNumber --value "$PKG_NAME" || true)
    case "$errno" in
    1 | EPERM) errno=EPERM ;;
    "" | 0 | -1 | kill | 2147483646) errno="kill" ;;
    esac
    state="$state SystemCallErrorNumber=$errno"
    # NoExecPaths=/: the process's / is mounted noexec.
    opts=$(awk '$5 == "/" { opts = $6 } END { print opts }' "/proc/$pid/mountinfo" 2>/dev/null || true)
    case ",$opts," in
    *,noexec,*) state="$state NoExecPaths=/" ;;
    *) state="$state NoExecPaths=" ;;
    esac
    # ExecPaths=/usr/bin/hwi-api: systemd binds the binary onto itself
    # without noexec, so the process's mount table has an entry for it,
    # and systemd reports the same path.
    opts=$(awk '$5 == "/usr/bin/hwi-api" { opts = $6; seen = 1 } END { if (seen) print opts; else print "none" }' "/proc/$pid/mountinfo" 2>/dev/null || true)
    case ",$opts," in
    ,none,) mounted="" ;;
    *,noexec,*) mounted="noexec" ;;
    *) mounted=/usr/bin/hwi-api ;;
    esac
    configured=$(systemctl show -p ExecPaths --value "$PKG_NAME" || true)
    if [ "$mounted" = "$configured" ]; then
        state="$state ExecPaths=$mounted"
    else
        state="$state ExecPaths=mount:$mounted,unit:$configured"
    fi
    printf '%s\n' "$state"
}

SANDBOX_ON="PrivateDevices=yes PrivateIPC=yes RemoveIPC=yes UMask=0077 SystemCallFilter=@system-service,~@privileged,~@resources SystemCallErrorNumber=EPERM NoExecPaths=/ ExecPaths=/usr/bin/hwi-api"
SANDBOX_OFF="PrivateDevices=no PrivateIPC=no RemoveIPC=no UMask=0022 SystemCallFilter=@system-service SystemCallErrorNumber=kill NoExecPaths= ExecPaths="

find_daemon 12
STATE=$(sandbox_state "$DAEMON_PID")
[ "$STATE" = "$SANDBOX_ON" ] \
    || fail 12 "the daemon (pid $DAEMON_PID) shows '$STATE', want '$SANDBOX_ON'"
log "assertion 12 OK: the daemon (pid $DAEMON_PID) shows $STATE"

# For the record, once: where this systemd puts the two calls that set a
# resource limit, and whether the shipped unit's resolved allow list keeps
# them. The Go runtime raises RLIMIT_NOFILE at start through prlimit64.
FILTER=" $(systemctl show -p SystemCallFilter --value "$PKG_NAME" || true) "
for call in prlimit64 setrlimit; do
    WHERE=""
    if command -v systemd-analyze >/dev/null 2>&1; then
        for set in @default @resources @privileged; do
            if systemd-analyze --no-pager syscall-filter "$set" 2>/dev/null | grep -qw "$call"; then
                WHERE="$WHERE $set"
            fi
        done
    else
        WHERE=" (systemd-analyze is not installed)"
    fi
    case "$FILTER" in
    *" $call "*) ALLOWED=allowed ;;
    *) ALLOWED="refused with EPERM" ;;
    esac
    log "$call is in:${WHERE:- none of @default @resources @privileged}; the shipped unit's filter: $ALLOWED"
done

# --- Assertion 13: the unit's sandbox costs no data ---------------------
# Every directive assertion 12 checks was chosen because no collector needs
# what it takes away. This proves it: the daemon runs once with all of them
# turned off and once under the shipped unit, and every subsystem's
# payload must come out the same. The "before" unit is a runtime drop-in,
# under /run: gone at reboot, removed below, never part of a package.
# Empty assignments reset the list directives; SystemCallErrorNumber=
# empty is the default, killing the process.
#
# A disk-backed mount under /home first, which a container has none of.
# block reads the host's mount table, /proc/1/mountinfo, and falls back to
# the daemon's own when it cannot read that one. Any directive that made
# block read the daemon's own would change the mount points it reports on a
# real host: ProtectHome= unmounts a separate /home, as Rocky's default
# layout has, in the daemon's namespace. ProtectProc= and PrivateUsers=
# were both considered and left out of the unit; this guards against any
# such directive. Without a mount under /home that change would not show
# here. The
# directory this script is in is a bind from the CI runner's disk; where it
# is not disk-backed, the mount is skipped and said so.
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT_DEV=$(stat -c %d "$SCRIPT_DIR")
if [ $(((SCRIPT_DEV >> 8) & 4095)) -ne 0 ]; then
    mkdir -p "$HOME_FIXTURE"
    mount --bind "$SCRIPT_DIR" "$HOME_FIXTURE" \
        || setup_fail "could not bind $SCRIPT_DIR to $HOME_FIXTURE"
    log "bound $SCRIPT_DIR (device $(((SCRIPT_DEV >> 8) & 4095)):$((SCRIPT_DEV & 255))) to $HOME_FIXTURE, a disk-backed mount under /home"
else
    log "$SCRIPT_DIR is not on a block device; no disk-backed mount under /home to compare block's mount points with"
fi

# exposure prints systemd-analyze security's verdict on the unit as loaded.
exposure() {
    if command -v systemd-analyze >/dev/null 2>&1; then
        systemd-analyze security --no-pager "$PKG_NAME.service" 2>&1 | tail -n 1
    else
        echo "unknown, systemd-analyze is not installed"
    fi
}

# restart_and_fetch restarts the daemon, waits for /readyz and fetches the
# inventory into the file $3, failing assertion $1 with $2 saying which
# unit it ran under.
restart_and_fetch() {
    systemctl restart "$PKG_NAME" >"$WORKDIR/restart.log" 2>&1 \
        || { cat "$WORKDIR/restart.log" >&2; fail "$1" "$PKG_NAME did not restart $2"; }
    wait_ready || fail "$1" "GET $OPS_URL/readyz did not return 200 within ${READYZ_TIMEOUT}s $2 (last status: $CODE)"
    CODE=$(fetch_inventory "$3" || true)
    [ "$CODE" = "200" ] || fail "$1" "GET $API_URL/api/v1/inventory returned $CODE $2, want 200"
    jq -e '.complete == true' "$3" >/dev/null \
        || fail "$1" ".complete was not true $2: $(jq -c '.complete, .errors, .disabled' "$3")"
}

# The comparison keeps each subsystem's payload, .data.<subsystem>.data,
# and the response's .host_id, .complete, .disabled and .errors. It drops
# the rest: each subsystem's collected_at and the response's own
# timestamps change on every collection. Within the
# payloads it deletes the fields that change on their own, between two
# collections under the same unit:
#   pci.devices[].link.current_speed  a PCIe link trains down when idle
#                                     (ASPM) and back up under load
#   pci.devices[].power_state         runtime power management moves an
#                                     idle device between D0 and D3hot
NORMALIZE='{host_id: .host_id, complete: .complete, disabled: .disabled, data: (.data | map_values(.data)), errors: .errors}
    | del(.data.pci.devices[]?.link.current_speed, .data.pci.devices[]?.power_state)'

# inventory_diff logs, at most 40 of them, the leaves in which the
# normalized inventories $1 and $2 differ: path, value in $1, value in $2.
inventory_diff() {
    jq -r -n --slurpfile a "$1" --slurpfile b "$2" '
        $a[0] as $x | $b[0] as $y
        | ([$x | paths(scalars)] + [$y | paths(scalars)] | unique)[] as $p
        | (try ($x | getpath($p)) catch null) as $u
        | (try ($y | getpath($p)) catch null) as $v
        | select($u != $v)
        | "\($p | map(tostring) | join(".")): \($u | tojson) -> \($v | tojson)"
    ' | head -n 40 | while IFS= read -r line; do log "  $line"; done
}

same_inventory() {
    jq -e -n --slurpfile a "$1" --slurpfile b "$2" '$a == $b' >/dev/null
}

mkdir -p "$BEFORE_DROPIN_DIR"
BEFORE_DROPIN_WRITTEN=1
cat >"$BEFORE_DROPIN" <<'EOF'
# Written by packaging/smoke_test.sh, assertion 13, and removed by it.
[Service]
PrivateDevices=no
PrivateIPC=no
RemoveIPC=no
UMask=0022
SystemCallFilter=
SystemCallFilter=@system-service
SystemCallErrorNumber=
NoExecPaths=
ExecPaths=
EOF
systemctl daemon-reload
log "exposure before (without the sandbox directives): $(exposure)"
restart_and_fetch 13 "under the 'before' drop-in" "$WORKDIR/before.json"
find_daemon 13
STATE=$(sandbox_state "$DAEMON_PID")
[ "$STATE" = "$SANDBOX_OFF" ] \
    || fail 13 "under the 'before' drop-in the daemon (pid $DAEMON_PID) shows '$STATE', want '$SANDBOX_OFF'; the comparison would not test every directive"
log "before: the daemon (pid $DAEMON_PID) shows $STATE"

rm -f "$BEFORE_DROPIN"
rmdir "$BEFORE_DROPIN_DIR" 2>/dev/null || true
systemctl daemon-reload
BEFORE_DROPIN_WRITTEN=0
log "exposure after (the shipped unit): $(exposure)"
restart_and_fetch 13 "under the shipped unit" "$WORKDIR/after.json"
find_daemon 13
STATE=$(sandbox_state "$DAEMON_PID")
[ "$STATE" = "$SANDBOX_ON" ] \
    || fail 13 "back under the shipped unit the daemon (pid $DAEMON_PID) shows '$STATE', want '$SANDBOX_ON'"

jq -S "$NORMALIZE" "$WORKDIR/before.json" >"$WORKDIR/before.norm.json"
jq -S "$NORMALIZE" "$WORKDIR/after.json" >"$WORKDIR/after.norm.json"
if ! same_inventory "$WORKDIR/before.norm.json" "$WORKDIR/after.norm.json"; then
    # A container's synthetic inventory, not a real host's: safe to print.
    log "the inventory differs without and with the sandbox directives (before -> after):"
    inventory_diff "$WORKDIR/before.norm.json" "$WORKDIR/after.norm.json"
    if command -v diff >/dev/null 2>&1; then
        diff -u "$WORKDIR/before.norm.json" "$WORKDIR/after.norm.json" | head -n 200 >&2 || true
    fi
    # Tells a directive's cost from a field that moves on its own: a field
    # that also differs between two runs of the same unit belongs in
    # NORMALIZE, with its reason.
    restart_and_fetch 13 "a second time under the shipped unit" "$WORKDIR/again.json"
    jq -S "$NORMALIZE" "$WORKDIR/again.json" >"$WORKDIR/again.norm.json"
    if same_inventory "$WORKDIR/after.norm.json" "$WORKDIR/again.norm.json"; then
        log "two runs under the shipped unit agree, so the difference above is the directives' doing"
    else
        log "two runs under the shipped unit differ as well (first -> second):"
        inventory_diff "$WORKDIR/after.norm.json" "$WORKDIR/again.norm.json"
    fi
    fail 13 "the inventory differs with the sandbox directives; see the differences above"
fi
if mountpoint -q "$HOME_FIXTURE"; then
    umount "$HOME_FIXTURE"
fi
rmdir "$HOME_FIXTURE" 2>/dev/null || true
log "assertion 13 OK: every subsystem's payload is the same without and with the sandbox directives ($(jq -r '.data | keys | length' "$WORKDIR/after.norm.json") subsystems), and $PKG_NAME runs under the shipped unit again"

# --- Assertion 14: the shipped unit starts without user namespaces -----
# Hardened hosts disable user namespaces with user.max_user_namespaces=0
# (the RHEL 8 STIG, some CIS profiles), and systemd refuses to start a unit
# with a directive that needs one: PrivateUsers=yes failed here with
# status=217/USER on systemd 252 and 255. So no directive in the unit may
# need one, and the shipped unit must start, become ready and serve a
# complete inventory with the limit at 0. The limit is per user namespace,
# and this privileged container shares the runner's, so cleanup restores it
# on any exit.
if [ -w "$USERNS_SYSCTL" ]; then
    USERNS_SAVED=$(cat "$USERNS_SYSCTL")
    log "restarting $PKG_NAME with user.max_user_namespaces=0 (was $USERNS_SAVED)"
    echo 0 >"$USERNS_SYSCTL" || setup_fail "could not write 0 to $USERNS_SYSCTL"
    restart_and_fetch 14 "with user namespaces disabled" "$WORKDIR/nouserns.json"
    STATE=$(systemctl is-active "$PKG_NAME" || true)
    [ "$STATE" = active ] || fail 14 "$PKG_NAME is '$STATE' with user namespaces disabled, want 'active'"
    log "$PKG_NAME is active, ready and serves a complete inventory with user namespaces disabled"
    echo "$USERNS_SAVED" >"$USERNS_SYSCTL"
    USERNS_SAVED=""
    restart_and_fetch 14 "once user namespaces were allowed again" "$WORKDIR/userns-restored.json"
    STATE=$(systemctl is-active "$PKG_NAME" || true)
    [ "$STATE" = active ] || fail 14 "$PKG_NAME is '$STATE' once user namespaces were allowed again, want 'active'"
    log "assertion 14 OK: $PKG_NAME starts and serves a complete inventory with user namespaces disabled, and again with user.max_user_namespaces restored to $(cat "$USERNS_SYSCTL")"
else
    log "$USERNS_SYSCTL is not writable here; assertion 14, starting without user namespaces, is skipped"
fi

# --- Assertion 9: reinstalling leaves config.yaml unmodified ------------
# reinstall_pkg (apt-get/dnf) does not return until the whole transaction -
# unpacking, and every maintainer scriptlet, including postinstall.sh's
# `systemctl daemon-reload`/`try-restart`, which are themselves synchronous
# D-Bus calls unless run with --no-block, which they are not - has
# finished. Unlike assertion 1, nothing here queries live systemd unit
# state through a separate call that could observe a not-yet-propagated
# transition: this reads a plain file's checksum off the local
# filesystem, and a package manager's own writes to it are visible to any
# read the instant that package manager's process has exited. So this one
# does not need to poll.
# Loosen the TLS directory first: the package must put it back to
# root:root 0700 on every install, keeping the files in it.
chown "$PKG_NAME":"$PKG_NAME" "$TLS_DIR"
chmod 0755 "$TLS_DIR"
TLS_SUM_BEFORE=$(cat "$TLS_DIR/cert.pem" "$TLS_DIR/key.pem" | sha256sum | awk '{print $1}')
log "reinstalling $PKG"
reinstall_pkg "$PKG"
CONFIG_SUM_AFTER_REINSTALL=$(sha256sum "$CONFIG_FILE" | awk '{print $1}')
[ "$CONFIG_SUM_AFTER_REINSTALL" = "$CONFIG_SUM_BEFORE" ] \
    || fail 9 "$CONFIG_FILE changed on reinstall (before: $CONFIG_SUM_BEFORE, after: $CONFIG_SUM_AFTER_REINSTALL)"
TLS_DIR_PERMS=$(stat -c '%a %U:%G' "$TLS_DIR" 2>/dev/null || echo missing)
[ "$TLS_DIR_PERMS" = "700 root:root" ] \
    || fail 9 "$TLS_DIR is '$TLS_DIR_PERMS' after reinstalling, want '700 root:root' restored by the package"
TLS_SUM_AFTER=$(cat "$TLS_DIR/cert.pem" "$TLS_DIR/key.pem" | sha256sum | awk '{print $1}')
[ "$TLS_SUM_AFTER" = "$TLS_SUM_BEFORE" ] \
    || fail 9 "the files in $TLS_DIR changed on reinstall"
log "assertion 9 OK: $CONFIG_FILE and the TLS files are unchanged after reinstalling, and $TLS_DIR is back to root:root 0700"

# --- Assertion 10: removing the package leaves config.yaml in place -----
# Same reasoning as assertion 9: remove_pkg blocks until apt-get/dnf's
# whole removal transaction - including preremove.sh's synchronous
# `systemctl disable`/`stop` - has completed, and this then checks a plain
# file's existence on the local filesystem, not a live systemd property
# through a second, separate query. No polling needed here either.
#
# dpkg and rpm keep the promise ("your edits survive removal") through two
# different, both correct, mechanisms - confirmed by actually running this
# against a real rpm removal, not assumed from the deb behaviour: dpkg's
# plain `remove` (as opposed to `purge`) never touches a conffile, so it
# stays at exactly $CONFIG_FILE. rpm's %config(noreplace) - what nfpm's
# `type: config|noreplace` maps to on this format - does something
# different on erase for a file it can tell was modified (which ours
# always is, since this script always overwrites the packaged default):
# it renames it to "$CONFIG_FILE.rpmsave" rather than leaving it under the
# original name. Both preserve the operator's edits; only dpkg preserves
# the exact path. Accepting only the literal path here would fail assertion
# 10 on every rpm run, correctly-behaving rpm and all - it very nearly did.
CONFIG_SAVED="$CONFIG_FILE"
if [ "$PKG_FORMAT" = rpm ]; then
    CONFIG_SAVED="$CONFIG_FILE.rpmsave"
fi
log "removing $PKG_NAME"
remove_pkg
[ -f "$CONFIG_SAVED" ] || fail 10 "neither $CONFIG_FILE nor $CONFIG_SAVED survived removing the package"
log "assertion 10 OK: the configuration survived removing the package (as $CONFIG_SAVED)"

# --- Assertion 11: postinstall refuses a symlink at the TLS path ---------
# install -d would follow a symlink to a directory and chown/chmod its
# target, so postinstall must refuse one instead, leaving the target
# alone. The TLS directory survived removal (the package does not own it);
# put a symlink to another directory in its place and install again. dpkg
# fails the install when postinstall fails and rpm only warns, so this
# checks what happened rather than the installer's exit status.
mv "$TLS_DIR" "$WORKDIR/tls-aside"
SHARED_DIR="$WORKDIR/shared-certs"
mkdir -p "$SHARED_DIR"
chmod 0755 "$SHARED_DIR"
ln -s "$SHARED_DIR" "$TLS_DIR"
log "installing $PKG with $TLS_DIR a symlink to another directory"
if [ "$PKG_FORMAT" = deb ]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$PKG" >"$WORKDIR/install-symlink.log" 2>&1 || true
else
    "$RPM_INSTALLER" install -y "$PKG" >"$WORKDIR/install-symlink.log" 2>&1 || true
fi
grep -q "$TLS_DIR exists but is not a directory" "$WORKDIR/install-symlink.log" \
    || { cat "$WORKDIR/install-symlink.log" >&2; fail 11 "postinstall did not refuse a symlink at $TLS_DIR"; }
SHARED_PERMS=$(stat -c '%a %U:%G' "$SHARED_DIR")
[ "$SHARED_PERMS" = "755 root:root" ] \
    || fail 11 "the symlink's target $SHARED_DIR became '$SHARED_PERMS', want it left at '755 root:root'"
[ -L "$TLS_DIR" ] || fail 11 "$TLS_DIR is no longer the symlink it was"
log "assertion 11 OK: postinstall refused a symlink at $TLS_DIR and left its target untouched"

log "all assertions passed"
