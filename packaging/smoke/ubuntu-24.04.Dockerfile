# The image the .deb smoke test boots, with systemd and the tools
# packaging/smoke_test.sh uses. CI builds it through the GitHub Actions
# cache, so an unchanged Dockerfile costs a cache restore, not a rebuild.
FROM ubuntu:24.04

# CI passes the date. Changing it rebuilds the package layer, so the apt
# lists kept below are at most a day old: smoke_test.sh installs the built
# .deb with apt-get, which would resolve any dependency it gains from these
# lists, and apt, unlike dnf, never refreshes stale ones on its own. The
# .deb has none today.
ARG REFRESH=unset

# The package lists are deliberately kept, not cleaned up as an image
# build usually would, for the same reason.
RUN echo "package refresh: ${REFRESH}" \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y systemd systemd-sysv dbus curl jq openssl socat
