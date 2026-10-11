#!/bin/sh
set -e
if ! getent group hwi-api >/dev/null; then
    groupadd --system hwi-api
fi
if ! getent passwd hwi-api >/dev/null; then
    useradd --system --gid hwi-api --no-create-home \
        --home-dir /nonexistent --shell /usr/sbin/nologin \
        --comment "hwi-api hardware discovery" hwi-api
fi
# The privileged helper's own user: it holds CAP_DAC_READ_SEARCH, so it is
# kept apart from the daemon's user.
if ! getent group hwi-api-helper >/dev/null; then
    groupadd --system hwi-api-helper
fi
if ! getent passwd hwi-api-helper >/dev/null; then
    useradd --system --gid hwi-api-helper --no-create-home \
        --home-dir /nonexistent --shell /usr/sbin/nologin \
        --comment "hwi-api privileged helper" hwi-api-helper
fi
