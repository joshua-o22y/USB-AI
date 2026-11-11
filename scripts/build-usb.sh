#!/usr/bin/env bash
set -euo pipefail

# Builds a ready-to-copy Universal USB-AI directory for Windows, macOS, and Linux.
OUT="${1:-./dist/USB-AI-Stick}"
mkdir -p "$OUT/.system/bin" "$OUT/.system/engines" "$OUT/.system/home" "$OUT/models" "$OUT/data/users"

echo "==> Building cross-OS USB-AI binaries into $OUT/.system/bin ..."
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o "$OUT/Start-USB-AI.exe" .
GOOS=windows GOARCH=arm64 go build -ldflags="-s -w" -o "$OUT/.system/bin/usb-ai-windows-arm64.exe" .
GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o "$OUT/.system/bin/usb-ai-darwin-arm64" .
GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o "$OUT/.system/bin/usb-ai-darwin-amd64" .
GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o "$OUT/.system/bin/usb-ai-linux-amd64" .
GOOS=linux   GOARCH=arm64 go build -ldflags="-s -w" -o "$OUT/.system/bin/usb-ai-linux-arm64" .

cat > "$OUT/Start-USB-AI.command" <<'EOF'
#!/usr/bin/env bash
DIR="$(cd "$(dirname "$0")" && pwd)"
ARCH="$(uname -m)"
if [ "$ARCH" = "arm64" ] || [ "$ARCH" = "aarch64" ]; then
  exec "$DIR/.system/bin/usb-ai-darwin-arm64" --root "$DIR"
else
  exec "$DIR/.system/bin/usb-ai-darwin-amd64" --root "$DIR"
fi
EOF
chmod +x "$OUT/Start-USB-AI.command"

cat > "$OUT/start-usb-ai.sh" <<'EOF'
#!/usr/bin/env bash
DIR="$(cd "$(dirname "$0")" && pwd)"
ARCH="$(uname -m)"
if [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
  exec "$DIR/.system/bin/usb-ai-linux-arm64" --root "$DIR" "$@"
else
  exec "$DIR/.system/bin/usb-ai-linux-amd64" --root "$DIR" "$@"
fi
EOF
chmod +x "$OUT/start-usb-ai.sh"

echo "==> Done! Copy the contents of $OUT onto any exFAT-formatted USB stick."
ls -lh "$OUT" "$OUT/.system/bin"
