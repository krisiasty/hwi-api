# The image the .rpm smoke test boots, with systemd and the tools
# packaging/smoke_test.sh uses. CI builds it through the GitHub Actions
# cache, so an unchanged Dockerfile costs a cache restore, not a rebuild.
FROM rockylinux:9

# CI passes the date, which rebuilds the package layer once a day, as for
# the .deb image. dnf refreshes expired metadata itself, but a daily
# rebuild keeps both images equally current.
ARG REFRESH=unset

# The dnf metadata is kept, so that smoke_test.sh's dnf install of the
# built .rpm would resolve any dependency it gains without fetching the
# repository metadata all over again. The .rpm has none today.
# procps-ng provides pgrep for smoke_test.sh's helper-instance check.
RUN echo "package refresh: ${REFRESH}" \
    && dnf install -y systemd jq openssl shadow-utils socat procps-ng
