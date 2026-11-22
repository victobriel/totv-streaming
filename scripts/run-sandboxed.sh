#!/bin/sh
# Runs a command (default: bin/totv-streaming) inside ONE transient systemd user scope with a ceiling on threads, RAM and swap, with no root: the cgroup
# is a child of the user's own delegated subtree (user@<uid>.service). The component also puts every SESSION in a scope of its own (TOTV_STREAM_CGROUP,
# cgroup.go); this wrapper is the outer ring: it bounds the component plus all its sessions, and it is what the tests use to run hostile cores safely.
#   scripts/run-sandboxed.sh                          # the component, 6 GiB / 1024 tasks
#   MEM=2G TASKS=300 scripts/run-sandboxed.sh go test -run TestCgroup .
# CPU is bounded too (CPU=400% = 4 cores). Nothing outside the user's own cgroup subtree is touched; nothing is installed.
set -eu
MEM="${MEM:-6G}" TASKS="${TASKS:-1024}" CPU="${CPU:-400%}"
here="$(cd "$(dirname "$0")/.." && pwd)"
[ $# -gt 0 ] || set -- "$here/bin/totv-streaming"
command -v systemd-run >/dev/null 2>&1 || { echo "systemd-run not found: no cgroup limits possible this way (use the unit in contrib/ or a container with pids_limit/mem_limit)" >&2; exit 69; }
[ -n "${XDG_RUNTIME_DIR:-}" ] || export XDG_RUNTIME_DIR="/run/user/$(id -u)"
exec systemd-run --user --scope --quiet --collect --description="totv-streaming (limited)" \
  -p MemoryMax="$MEM" -p MemorySwapMax=0 -p TasksMax="$TASKS" -p CPUQuota="$CPU" -p OOMPolicy=kill -- "$@"
