#!/data/data/com.termux/files/usr/bin/bash
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
: "${PREFIX:?Jalankan installer ini di Termux (PREFIX belum tersedia).}"
command -v node >/dev/null || { echo 'Install Node.js: pkg install nodejs-lts' >&2; exit 1; }
command -v go >/dev/null || { echo 'Install Go: pkg install golang' >&2; exit 1; }

# Do not overwrite an unrelated system/service manager command.
DEST="$PREFIX/bin/service"
if [[ -e "$DEST" ]] && ! grep -q '# opencode-router-service' "$DEST"; then
  echo "$DEST sudah ada dan bukan milik repo ini; tidak ditimpa." >&2
  exit 1
fi

cd "$ROOT"
echo 'Build opencode-router...'
# Build to a temporary file so an existing running binary is not overwritten.
TMP="$(mktemp "$ROOT/.ocr-build.XXXXXX")"
trap 'rm -f "$TMP"' EXIT
go build -o "$TMP" ./cmd/ocr
chmod 700 "$TMP"
mv -f "$TMP" "$ROOT/ocr"

# Bash %q preserves repo paths containing spaces or shell metacharacters.
{
  printf '#!%s/bin/bash\n' "$PREFIX"
  printf '# opencode-router-service\n'
  printf 'exec %q %q "$@"\n' "$(command -v node)" "$ROOT/scripts/service.cjs"
} > "$DEST"
chmod 700 "$DEST"
echo 'Terpasang! Gunakan: service start | stop | restart | status | logs'
