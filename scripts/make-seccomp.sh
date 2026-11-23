#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 victobriel
# The base profile comes from moby/profiles (Apache-2.0, Copyright the Moby/Docker authors); the added rules are GPL-3.0-or-later. See NOTICE.md.
# Regenerates docker/seccomp-totv.json = Docker's default seccomp profile (moby/profiles, fetched, review before committing)
# + exactly three extra rules the component's child needs to build its own namespaces (see README "Docker"):
#   unshare(flags)  only with flags a subset of CLONE_NEWUSER|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWIPC (0x10000000|0x40000000|0x00020000|0x08000000)
#   mount(.., flags) only with flags == MS_REC|MS_PRIVATE (0x44000) or MS_NOSUID|MS_NODEV|MS_NOEXEC (0x0e): no bind, remount, move
# LIMIT of the mount rule: seccomp sees only the raw arguments, and the file system type is a POINTER (arg 2), so the profile cannot say "tmpfs only".
# What bounds it: the rule needs CAP_SYS_ADMIN in the child's own user namespace, and the child drops every capability and denies mount/unshare/setns
# for the core with its own filter before any core code runs (emu/host.c), so the only mounts that ever happen are the two the host itself does.
# The base profile is PINNED to a commit of moby/profiles (the branch moves): a different commit is a conscious update, review the diff.
# usage: scripts/make-seccomp.sh [moby/profiles commit] > docker/seccomp-totv.json
set -eu
ref=${1:-a0fa70805f742f7da15b367770d802ea2a86c68b}
case $ref in [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;; *) echo "pass a commit SHA of moby/profiles, not a branch name" >&2; exit 2 ;; esac
curl -fsSL "https://raw.githubusercontent.com/moby/profiles/$ref/seccomp/default.json" | python3 -I -c '
import json, sys
p = json.load(sys.stdin)
add = lambda names, args, comment: p["syscalls"].append({"names": names, "action": "SCMP_ACT_ALLOW", "args": args, "comment": comment})
add(["unshare"], [{"index": 0, "value": 0xFFFFFFFF & ~(0x10000000 | 0x40000000 | 0x00020000 | 0x08000000), "valueTwo": 0, "op": "SCMP_CMP_MASKED_EQ"}],
    "totv: only user/net/mount/ipc namespaces")
add(["mount"], [{"index": 3, "value": 0x44000, "op": "SCMP_CMP_EQ"}], "totv: mount(MS_REC|MS_PRIVATE) in the child mount namespace")
add(["mount"], [{"index": 3, "value": 0x0e, "op": "SCMP_CMP_EQ"}], "totv: mount tmpfs nosuid,nodev,noexec on the private /dev/shm")
json.dump(p, sys.stdout, indent=2); print()
'
