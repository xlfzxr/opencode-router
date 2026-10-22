#!/data/data/com.termux/files/usr/bin/bash
# Start Tor with a local-only SOCKS listener for opencode-router's `tor` pool entry.
set -euo pipefail

TOR_DIR="${HOME}/.tor"
mkdir -p "$TOR_DIR/data"
chmod 700 "$TOR_DIR" "$TOR_DIR/data"

cat > "$TOR_DIR/torrc" <<EOF
SocksPort 9050
SocksPolicy accept 127.0.0.1
DataDirectory $TOR_DIR/data
Log notice file $TOR_DIR/tor.log
RunAsDaemon 0
EOF

if pgrep -x tor >/dev/null 2>&1; then
  echo "tor already running"
  exit 0
fi

nohup tor -f "$TOR_DIR/torrc" > "$TOR_DIR/stdout.log" 2>&1 &
echo "tor starting (cold bootstrap can take a few minutes on mobile)..."

for _ in $(seq 1 90); do
  sleep 2
  if grep -q "Bootstrapped 100" "$TOR_DIR/tor.log" 2>/dev/null; then
    echo "tor bootstrapped; SOCKS5 on 127.0.0.1:9050"
    exit 0
  fi
done

echo "tor did not bootstrap within 180s; see $TOR_DIR/tor.log" >&2
exit 1
