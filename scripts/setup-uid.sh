#!/usr/bin/env bash
# One-time setup of a DEDICATED unprivileged uid for totv-streaming (GPL-3.0-or-later). Needs root. Not run by anything in this repository.
# See README.md, "GPU isolation (opção 2)": what a dedicated uid buys (a compromised core can no longer touch the Totv's processes, files, ssh keys,
# browser profile or the desktop session: they belong to another uid) and what it does not (a kernel exploit through the GPU driver is root).
#
#   sudo scripts/setup-uid.sh --totv-user <login of the user that runs the Totv> [--dry-run] [--gen-secret]
#
# What it does, and nothing else:
#   1. group + user "totv-stream" (system, no login shell, no home), in the groups video and render (GPU nodes on distributions that restrict them)
#   2. the Totv's user joins group totv-stream: that is the ONLY way the Totv reaches the control socket (0660 totv-stream) and the only reason the
#      secret file is readable by the component
#   3. /etc/totv-stream/retro-stream.secret: owned by the Totv's user (it writes/rotates the secret), group totv-stream may read (0640)
#   4. empty mount points /var/lib/totv-stream/{tmp,rw} for the sized tmpfs of the unit (TemporaryFileSystem= needs the directory to exist)
# It does NOT touch device permissions, udev, the unit, the desktop or the Totv. Re-running is safe (everything is "ensure").
set -euo pipefail

TOTV_USER="" DRY=0 GEN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --totv-user) TOTV_USER="${2:-}"; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    --gen-secret) GEN=1; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done
[ -n "$TOTV_USER" ] || { echo "--totv-user <login> is required (the user that runs the Totv)" >&2; exit 64; }
if [ "$DRY" = 0 ] && [ "$(id -u)" != 0 ]; then echo "run as root (or use --dry-run to only print)" >&2; exit 77; fi
case "$TOTV_USER" in -*|*[!A-Za-z0-9._-]*|[0-9]*) echo "invalid --totv-user (must be a login name: no leading '-', no digits first, only [A-Za-z0-9._-]): $TOTV_USER" >&2; exit 64 ;; esac
getent passwd -- "$TOTV_USER" >/dev/null 2>&1 || { echo "no such user: $TOTV_USER" >&2; exit 65; }

run() { if [ "$DRY" = 1 ]; then printf '+ %s\n' "$*"; else "$@"; fi; }

# 1. group and user
getent group totv-stream >/dev/null || run groupadd --system totv-stream
getent passwd totv-stream >/dev/null || run useradd --system --gid totv-stream --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment "totv-streaming" totv-stream
for g in video render; do
  if getent group "$g" >/dev/null; then run usermod -aG "$g" totv-stream
  else echo "note: group '$g' does not exist here; remove it from SupplementaryGroups= in the unit if the GPU nodes are world-accessible" >&2; fi
done

# 2. the Totv's user may talk to the component
run usermod -aG totv-stream "$TOTV_USER"
echo "note: $TOTV_USER must log in again (or restart the Totv's service) for the new group to apply" >&2

# 3. the shared secret: written by the Totv (owner), read by the component (group)
run install -d -m 0755 -o root -g root /etc/totv-stream
if [ ! -e /etc/totv-stream/retro-stream.secret ]; then
  if [ "$GEN" = 1 ]; then
    if [ "$DRY" = 1 ]; then echo '+ (openssl rand -hex 32) > /etc/totv-stream/retro-stream.secret'; else umask 077; openssl rand -hex 32 > /etc/totv-stream/retro-stream.secret; fi
  else run install -m 0640 /dev/null /etc/totv-stream/retro-stream.secret; fi
fi
run chown "$TOTV_USER":totv-stream /etc/totv-stream/retro-stream.secret
run chmod 0640 /etc/totv-stream/retro-stream.secret

# 4. mount points of the sized tmpfs (empty directories, read-only for the service otherwise)
run install -d -m 0755 -o root -g root /var/lib/totv-stream /var/lib/totv-stream/tmp /var/lib/totv-stream/rw

# what is left to the operator, printed instead of done
cat >&2 <<MSG

GPU nodes on this machine (the unit's DeviceAllow= is the gate; these modes matter only if you also restrict them with udev):
$(ls -l /dev/nvidiactl /dev/nvidia0 /dev/dri/renderD128 2>/dev/null || true)
Not done here, by choice: changing the modes of /dev/nvidia* or /dev/dri/* (the desktop's Xorg/Wayland session uses the same nodes).

Next (as root):
  install -d /opt/totv-streaming && cp -a bin cores /opt/totv-streaming/ && chown -R root:root /opt/totv-streaming
  install -m 0644 contrib/totv-streaming.service /etc/systemd/system/ && systemctl daemon-reload
  systemctl enable --now totv-streaming
  curl --unix-socket /run/totv-stream/control.sock -H "Authorization: Bearer \$(cat /etc/totv-stream/retro-stream.secret)" http://x/v1/health   # as $TOTV_USER, after re-login
MSG
