#!/usr/bin/env bash
# Run with Bash (Linux or Git Bash). Never starts a server.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
usage() { printf 'Usage: bash tools/build-release.sh [--version LABEL]\n'; }
LABEL=""
while (($#)); do
  case "$1" in
    --version) (($# >= 2)) || { usage >&2; exit 2; }; LABEL=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
for cmd in go node npm tar sha256sum mktemp; do
  command -v "$cmd" >/dev/null || { printf 'Missing command: %s\n' "$cmd" >&2; exit 1; }
done
REQUIRED_GO=""
while read -r key value rest; do
  if [[ $key == go ]]; then REQUIRED_GO=$value; break; fi
done < go.mod
[[ -n $REQUIRED_GO ]] || { printf 'No go directive in go.mod\n' >&2; exit 1; }
# go env resolves GOTOOLCHAIN=auto, if configured. Do not silently rewrite go.mod.
ACTUAL_GO=$(go env GOVERSION)
printf 'go.mod requires Go %s; selected %s\n' "$REQUIRED_GO" "$ACTUAL_GO"
node --input-type=commonjs -e '
const v = process.versions.node.split(".").map(Number);
if (!((v[0] === 20 && v[1] >= 19) || (v[0] === 22 && v[1] >= 12) || v[0] > 22)) {
  console.error("Vite 7 requires Node 20.19+ or 22.12+ (22/24 LTS recommended)"); process.exit(1);
}'
if [[ -z $LABEL ]]; then LABEL="local-$(date -u +%Y%m%dT%H%M%SZ)"; fi
[[ $LABEL =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] || { printf 'Invalid release label\n' >&2; exit 2; }
NAME="pve-forward-$LABEL"
mkdir -p "$ROOT/release"
[[ ! -e "$ROOT/release/$NAME" && ! -e "$ROOT/release/$NAME.tar.gz" && ! -e "$ROOT/release/$NAME.tar.gz.sha256" ]] || {
  printf 'Release already exists; use a new label (nothing overwritten).\n' >&2; exit 1;
}
STAGE=$(mktemp -d "$ROOT/release/.build-XXXXXXXX")
trap 'rm -rf -- "$STAGE"' EXIT
PKG="$STAGE/$NAME"
mkdir "$PKG"
# Frontend must be built first: web/embed.go embeds web/dist into the controller.
npm --prefix "$ROOT/web" ci --no-audit --no-fund
npm --prefix "$ROOT/web" run build
[[ -s "$ROOT/web/dist/index.html" ]] || { printf 'Frontend output missing\n' >&2; exit 1; }
export CGO_ENABLED=0 GOOS=linux
for arch in amd64 arm64; do
  printf 'Building Linux %s controller and agent...\n' "$arch"
  GOARCH=$arch go build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w' -o "$PKG/pve-forward-linux-$arch" ./cmd/pve-forward
  GOARCH=$arch go build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w' -o "$PKG/pve-forward-agent-linux-$arch" ./cmd/pve-forward-agent
done
cp "$ROOT/install.sh" "$ROOT/README.md" "$PKG/"
chmod 755 "$PKG/install.sh" "$PKG"/pve-forward-*
{
  printf 'Release label: %s\nBuilt UTC: %s\nGo directive: %s\nToolchain: %s\n' "$LABEL" "$(date -u +%FT%TZ)" "$REQUIRED_GO" "$ACTUAL_GO"
  printf 'Node: %s\nNpm: %s\nCGO_ENABLED=0; GOOS=linux; GOARCH=amd64,arm64\n' "$(node --version)" "$(npm --version)"
  printf 'Label is not a version override; application version is defined in Go source.\n'
} > "$PKG/BUILD-INFO.txt"
(
  cd "$PKG"
  sha256sum pve-forward-linux-amd64 pve-forward-linux-arm64 pve-forward-agent-linux-amd64 pve-forward-agent-linux-arm64 install.sh README.md BUILD-INFO.txt > SHA256SUMS
)
tar -czf "$STAGE/$NAME.tar.gz" -C "$STAGE" "$NAME"
(cd "$STAGE" && sha256sum "$NAME.tar.gz" > "$NAME.tar.gz.sha256")
mv "$PKG" "$ROOT/release/$NAME"
mv "$STAGE/$NAME.tar.gz" "$STAGE/$NAME.tar.gz.sha256" "$ROOT/release/"
printf 'Ready: %s/release/%s.tar.gz\nChecksums: archive .sha256 and package SHA256SUMS\n' "$ROOT" "$NAME"
