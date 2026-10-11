#!/bin/sh
set -e
# nfpm passes the package manager's native argument through as $1, and the
# two managers use different conventions for "this is an upgrade, not a
# real removal":
#   rpm  (%preun): 0 = final removal (last copy going away), 1 = upgrade
#   dpkg (prerm):  "remove" or "purge" = final removal;
#                  "upgrade", "failed-upgrade" or "deconfigure" = upgrade
#                  in progress -- the new package's files land right after
#
# On an upgrade this script must do nothing: stopping the service here
# would leave the host without hardware discovery until a human noticed,
# since nothing would ever start it back up (postinstall only try-restarts
# an already-running unit). Only a genuine removal disables and stops it.
#
# /etc/hwi-api (configuration and any TLS material an operator placed
# there) is deliberately left alone in every case -- it must survive a
# package removal.
case "$1" in
    0|remove|purge)
        if command -v systemctl >/dev/null 2>&1; then
            systemctl disable hwi-api.service || true
            systemctl stop hwi-api.service || true
            systemctl stop hwi-api-helper.socket || true
        fi
        ;;
    *)
        # upgrade, failed-upgrade, deconfigure, or an rpm "1": leave the
        # running service alone.
        ;;
esac
