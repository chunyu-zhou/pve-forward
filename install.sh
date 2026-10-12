#!/usr/bin/env bash
# Non-interactive controller installer. Local, unpacked release only; no downloads.
set -Eeuo pipefail
umask 077
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
usage() {
  printf '%s\n' 'Usage: sudo bash install.sh [--release-dir DIR] [--listen ADDR] [--tls self|acme|proxy]' \
    'Default release directory: directory containing this script.' \
    '--listen / --tls are first-install overrides only; upgrades preserve the unit.' \
    'Fixed data path: /var/lib/pve-forward. No packages are installed, no data is deleted.'
}
SOURCE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
LISTEN="" TLS=""
while (($#)); do
  case "$1" in
    --release-dir|--listen|--tls)
      (($# >= 2)) && [[ -n $2 ]] || fail "Missing value for $1"
      case "$1" in --release-dir) SOURCE=$2;; --listen) LISTEN=$2;; --tls) TLS=$2;; esac
      shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; fail "Unknown argument: $1" ;;
  esac
done
[[ $(uname -s) == Linux ]] || fail 'Installer requires Linux with systemd (build script also supports Git Bash).'
[[ $EUID == 0 ]] || fail 'Run as root via sudo bash install.sh.'
[[ -d /run/systemd/system ]] || fail 'systemd must be running.'
for cmd in systemctl sha256sum tar install mktemp readlink flock cmp stat; do
  command -v "$cmd" >/dev/null || fail "Missing command: $cmd"
done
case "$(uname -m)" in x86_64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) fail 'Supported architectures: amd64, arm64';; esac
[[ -z $TLS || $TLS == self || $TLS == acme || $TLS == proxy ]] || fail 'Invalid TLS mode.'
# Restrict characters interpolated into the systemd unit (no quotes, %, $, whitespace).
if [[ -n $LISTEN ]]; then
  [[ $LISTEN =~ ^([A-Za-z0-9.-]*|\[[0-9A-Fa-f:.]+\]):([0-9]{1,5})$ ]] || fail 'Listen address must be host:port, :port or [IPv6]:port.'
  PORT=${BASH_REMATCH[2]}
  ((10#$PORT >= 1 && 10#$PORT <= 65535)) || fail 'Listen port must be 1-65535.'
fi
SOURCE=$(cd -- "$SOURCE" && pwd)
BASE=/opt/pve-forward
DATA=/var/lib/pve-forward
BACKUPS=/var/backups/pve-forward
UNIT=/etc/systemd/system/pve-forward.service
BIN=/usr/local/bin/pve-forward
CURRENT=$BASE/current
SERVICE=pve-forward.service
exec 9>/run/lock/pve-forward-install.lock
flock -n 9 || fail 'Another install is running.'
# Refuse unmanaged paths instead of overwriting somebody else’s installation.
for path in "$BASE" "$BASE/releases" "$DATA" "$BACKUPS" "$UNIT" "$DATA/data.db" "$DATA/data.db-wal" "$DATA/data.db-shm" "$DATA/master.key"; do
  [[ ! -L $path ]] || fail "Symlink not allowed: $path"
done
for path in "$BASE" "$BASE/releases" "$DATA" "$BACKUPS"; do
  [[ ! -e $path || -d $path ]] || fail "Not a directory: $path"
done
if [[ -e $BASE && ! -f $BASE/.installer-v1 ]]; then fail "Unmanaged $BASE; migrate manually first."; fi
if [[ -e $UNIT && ! -f $BASE/.installer-v1 ]]; then fail "Unmanaged $UNIT; migrate manually first."; fi
if [[ -e $BIN || -L $BIN ]]; then
  [[ -L $BIN && $(readlink "$BIN") == "$CURRENT/pve-forward" ]] || fail "Refusing to overwrite $BIN"
fi
if [[ -e $CURRENT || -L $CURRENT ]]; then
  [[ -L $CURRENT ]] || fail "$CURRENT is not a symlink."
  OLD=$(readlink "$CURRENT")
  [[ $OLD == "$BASE/releases/"* && -d $OLD ]] || fail 'Invalid current release link.'
else
  OLD=""
fi
LOAD=$(systemctl show -p LoadState --value "$SERVICE")
[[ $LOAD == not-found || -f $UNIT ]] || fail 'An existing unit is loaded from another location; migrate manually.'
if [[ -f $UNIT && ( -n $LISTEN || -n $TLS ) ]]; then
  fail 'Upgrade preserves the existing unit; change overrides via systemctl edit instead.'
fi
# The application creates a new key if missing: never allow this over an existing DB.
if [[ -e $DATA/data.db || -e $DATA/data.db-wal || -e $DATA/data.db-shm ]]; then
  [[ -f $DATA/data.db && -s $DATA/data.db && -f $DATA/master.key ]] || fail 'Incomplete data directory; restore database and original master.key together.'
fi
if [[ -e $DATA/master.key ]]; then
  [[ ! -L $DATA/master.key && $(stat -c %s "$DATA/master.key") == 32 ]] || fail 'master.key must be the original 32-byte regular file.'
fi
for arch in amd64 arm64; do
  [[ ! -e $DATA/agent/pve-forward-agent-linux-$arch && ! -L $DATA/agent/pve-forward-agent-linux-$arch ]] || fail "Data-directory agent override found ($arch); back it up and move it aside to use release agents."
done
FILES=(pve-forward-linux-amd64 pve-forward-linux-arm64 pve-forward-agent-linux-amd64 pve-forward-agent-linux-arm64 install.sh README.md BUILD-INFO.txt)
for file in "${FILES[@]}" SHA256SUMS; do
  [[ -f $SOURCE/$file && ! -L $SOURCE/$file ]] || fail "Missing regular release file: $file"
done
# Only checksum explicit basenames. Never feed arbitrary paths from a manifest to root tools.
MANIFEST=$(cd "$SOURCE" && sha256sum "${FILES[@]}")
[[ $MANIFEST == "$(< "$SOURCE/SHA256SUMS")" ]] || fail 'Release checksum mismatch (expected build-release.sh layout).'
HASH=$(printf '%s\n' "$MANIFEST" | sha256sum)
HASH=${HASH%% *}
TARGET=$BASE/releases/$HASH
WORK="" BACKUP="" STOPPED=0 SWITCHED=0 NEW_UNIT=0
cleanup() { [[ -z $WORK ]] || rm -rf -- "$WORK"; }
on_error() {
  local rc=$?
  trap - ERR INT TERM
  set +e
  if ((STOPPED)); then
    systemctl stop "$SERVICE"
    if ((SWITCHED)); then
      if [[ -n $OLD ]]; then
        ln -s "$OLD" "$WORK/rollback-current"
        mv -Tf "$WORK/rollback-current" "$CURRENT"
      else
        rm -f "$CURRENT"
      fi
    fi
    if ((NEW_UNIT)); then systemctl disable "$SERVICE"; rm -f "$UNIT"; systemctl daemon-reload; fi
    printf 'Install failed; service left STOPPED. Data was NOT rolled back or deleted.\nBackup: %s\nInspect journalctl -u %s before recovery; do not run an old binary on a migrated database.\n' "${BACKUP:-none (new install)}" "$SERVICE" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT
trap on_error ERR
trap 'false' INT TERM
install -d -m 755 "$BASE" "$BASE/releases"
install -d -m 700 "$BACKUPS"
WORK=$(mktemp -d "$BASE/.install-XXXXXXXX")
mkdir "$WORK/payload"
for file in "${FILES[@]}" SHA256SUMS; do install -m 644 "$SOURCE/$file" "$WORK/payload/$file"; done
# Verify the copied payload, not only the source (also catches copy-time changes).
COPIED=$(cd "$WORK/payload" && sha256sum "${FILES[@]}")
[[ $COPIED == "$MANIFEST" ]] || fail 'Copied payload checksum mismatch.'
install -m 755 "$WORK/payload/pve-forward-linux-$ARCH" "$WORK/payload/pve-forward"
chmod 755 "$WORK/payload" "$WORK/payload"/pve-forward-agent-linux-*
"$WORK/payload/pve-forward" version
if [[ -e $TARGET ]]; then
  [[ -d $TARGET && ! -L $TARGET ]] || fail 'Invalid release destination.'
  for file in "${FILES[@]}" SHA256SUMS pve-forward; do cmp "$WORK/payload/$file" "$TARGET/$file" || fail 'Existing immutable release differs.'; done
else
  mv "$WORK/payload" "$TARGET"
fi
# Mark ownership only after verified payload is ready. No running process is touched above.
printf 'pve-forward installer v1\n' > "$BASE/.installer-v1"
STOPPED=1
if [[ $LOAD != not-found ]]; then systemctl stop "$SERVICE"; fi
# Consistent SQLite backup: all DB/WAL/SHM, key, TLS and other data, after service stop.
BACKUP=$(mktemp -d "$BACKUPS/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXXXX")
if [[ -d $DATA ]]; then tar -cpf "$BACKUP/data.tar" -C "$DATA" .; fi
if [[ -f $UNIT ]]; then cp -a "$UNIT" "$BACKUP/pve-forward.service"; fi
if [[ -d $UNIT.d ]]; then cp -a "$UNIT.d" "$BACKUP/pve-forward.service.d"; fi
printf '%s\n' "$OLD" > "$BACKUP/previous-release"
install -d -m 700 "$DATA"
[[ ! -f $DATA/master.key ]] || chmod 600 "$DATA/master.key"
if [[ ! -f $UNIT ]]; then
  NEW_UNIT=1
  {
    printf '%s\n' '[Unit]' 'Description=PVE Forward Controller' 'After=network-online.target' 'Wants=network-online.target' '' '[Service]' 'Type=simple' 'User=root' 'UMask=0077'
    printf 'ExecStart=%s/pve-forward run -data %s' "$CURRENT" "$DATA"
    [[ -z $LISTEN ]] || printf ' -listen %s' "$LISTEN"
    [[ -z $TLS ]] || printf ' -tls %s' "$TLS"
    printf '\n'
    printf '%s\n' 'Restart=on-failure' 'RestartSec=5' 'TimeoutStopSec=30' 'NoNewPrivileges=true' 'PrivateTmp=true' 'ProtectHome=true' 'ProtectSystem=strict' "ReadWritePaths=$DATA" 'CapabilityBoundingSet=CAP_NET_BIND_SERVICE' '' '[Install]' 'WantedBy=multi-user.target'
  } > "$WORK/pve-forward.service"
  install -m 644 "$WORK/pve-forward.service" "$UNIT"
fi
ln -s "$TARGET" "$WORK/next-current"
mv -Tf "$WORK/next-current" "$CURRENT"
SWITCHED=1
if [[ ! -L $BIN ]]; then ln -s "$CURRENT/pve-forward" "$BIN"; fi
systemctl daemon-reload
systemctl enable "$SERVICE"
systemctl restart "$SERVICE"
# Basic process liveness only, not HTTP/TLS, PVE or network acceptance.
sleep 3
systemctl is-active --quiet "$SERVICE"
STOPPED=0
printf '\nInstalled: %s\nBackup: %s\n' "$TARGET" "$BACKUP"
printf '%s\n' 'Default first access: https://<controller-IP>:5000 (self-signed, unless overridden).' \
  'Initialization token: sudo cat /var/lib/pve-forward/setup-token' \
  'Logs: sudo journalctl -u pve-forward.service -n 80 --no-pager' \
  'The token file is removed after setup. This installer never resets it.' \
  'Before initialization only, regenerate a lost token with:' \
  '  sudo systemctl stop pve-forward.service' \
  '  sudo /usr/local/bin/pve-forward setup-token -data /var/lib/pve-forward' \
  '  sudo systemctl start pve-forward.service' \
  'Verify HTTPS and /agent/download/{amd64,arm64} yourself before adding nodes.'
