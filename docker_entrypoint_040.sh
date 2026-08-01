#!/bin/sh
#
# The StartOS 0.4.x entrypoint.
#
# **Thin on purpose.** Everything about rendering the configuration is in
# docker_entrypoint.sh, which every deployment runs — compose, Umbrel, StartOS
# 0.3.5.1 and this one. One renderer means a settings bug cannot be
# platform-specific, and the platform this is hardest to reproduce on is the one
# a user is most likely to be running.
#
# What differs on 0.4.x is only what happens *after* the configuration exists.
# There, the package's own compiled TypeScript is the supervisor: `sdk.Daemons`
# starts each process, watches it, restarts it and reports its health to the
# platform separately. So this script renders and stops, and the daemons are
# started by main.ts rather than by s6 — which is what lets the StartOS UI show
# the second Bitcoin node still syncing while the dashboard is already up,
# instead of one green dot for both.
#
# s6-overlay is still in the image, and still used on 0.3.5.1, which has no
# equivalent API and exactly one supervised image.

set -eu

exec /usr/local/bin/docker_entrypoint.sh --render-only "$@"
