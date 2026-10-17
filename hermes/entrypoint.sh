#!/bin/sh
# Container entrypoint wrapper for the Boaky deployment.
#
# Why this exists: docker-compose mounts ./hermes as a *directory* at
# /opt/hermes-config (read-only) instead of bind-mounting config.yaml and .env
# as individual files. A bind-mounted file is resolved once, when the container
# starts, so an editor that writes atomically (temp file + rename) leaves the
# running container pinned to the old inode: it keeps serving the previous
# key/config until the container is recreated. A directory mount always reflects
# the current files, and this script copies them into HERMES_HOME on every boot.
#
# `./hermes/config.yaml` in the repo is therefore the source of truth. Hermes
# still writes to its own copy under HERMES_HOME (e.g. `hermes config set`), but
# that copy is overwritten on the next boot — edit the repo file to persist.
#
# Invoked as the image entrypoint, so "$@" is the container CMD and is forwarded
# unchanged to the stock dispatcher.

set -e

for f in config.yaml .env; do
    src="/opt/hermes-config/$f"
    [ -f "$src" ] || continue
    cp -f "$src" "/opt/data/$f"
    chmod 600 "/opt/data/$f"
done

exec /opt/hermes/docker/entrypoint-dispatch.sh "$@"
