#!/bin/sh
set -e
# The unit loads every file in this directory as a TLS credential and will
# not start if the directory is missing. Create it on a fresh install; on
# every install and upgrade, install -d also resets an existing one to
# root:root 0700 without touching its files. Only systemd, as root, reads
# it, and an owner other than root could swap in their own key.
#
# Refuse anything else at that path first. install -d follows a symlink
# to a directory and would chown and chmod its target, some other
# directory entirely; for a file or anything else it fails with a message
# that does not say what to do.
tls_dir=/etc/hwi-api/tls
if [ -L "$tls_dir" ] || { [ -e "$tls_dir" ] && [ ! -d "$tls_dir" ]; }; then
    echo "hwi-api: $tls_dir exists but is not a directory (a symlink or another file type);" >&2
    echo "hwi-api: move it aside and reinstall; the unit loads the TLS certificate and key from this directory" >&2
    exit 1
fi
install -d -m 0700 -o root -g root "$tls_dir"
# Versions before the privileged helper copied the root-only DMI files into
# this directory at each start; nothing reads it any more. The path is fixed
# and root-owned, so removing it cannot be steered elsewhere.
rm -rf /run/hwi-api-dmi
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    # try-restart only acts on a unit that is already running: a no-op on
    # a fresh install (the packaged config.yaml deliberately fails to load
    # until an operator supplies TLS material and populates
    # api.access.allow, so the unit is never enabled or started here --
    # see the note in preremove.sh), and on an upgrade it swaps the
    # running process onto the newly installed binary without ever
    # leaving the host without an agent running.
    # Belt and braces: the restarted agent's unit wants the helper's
    # socket, but start it here too, so a running agent always has it.
    systemctl is-active --quiet hwi-api.service && systemctl start hwi-api-helper.socket || true
    systemctl try-restart hwi-api.service || true
fi
