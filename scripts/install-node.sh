#!/usr/bin/env bash
set -euo pipefail

# Lightweight systemd installer for a mailbox-tunnel exit node (ideal for 512MB / NAT VPS).
REPO="lucyna-aqu/mailbox-tunnel"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/mailbox-tunnel"
SERVICE_FILE="/etc/systemd/system/mailbox-tunnel-node.service"

if [ "$(id -u)" -ne 0 ]; then
  echo "Please run as root (e.g. sudo bash install-node.sh)"
  exit 1
fi

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) BIN_NAME="mailbox-tunnel-linux-amd64" ;;
  aarch64|arm64) BIN_NAME="mailbox-tunnel-linux-arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

echo "Downloading latest ${BIN_NAME} from GitHub Releases..."
URL="https://github.com/${REPO}/releases/latest/download/${BIN_NAME}"
curl -fL "$URL" -o "${INSTALL_DIR}/mailbox-tunnel"
chmod +x "${INSTALL_DIR}/mailbox-tunnel"

mkdir -p "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"

ENV_FILE="${CONFIG_DIR}/node.env"
if [ ! -f "$ENV_FILE" ]; then
  read -rp "Apps Script Deployment ID (for node registration): " DEPLOYMENT_ID
  read -rsp "Apps Script AUTH_KEY: " AUTH_KEY
  echo
  NODE_KEY="$("${INSTALL_DIR}/mailbox-tunnel" -mode keygen | awk '/node key/{print $NF}')"
  cat > "$ENV_FILE" <<EOF
MAILBOX_DEPLOYMENT_ID=${DEPLOYMENT_ID}
MAILBOX_AUTH_KEY=${AUTH_KEY}
MAILBOX_NODE_KEY=${NODE_KEY}
STATUS_LISTEN=127.0.0.1:7860
EOF
  chmod 600 "$ENV_FILE"
  echo "Saved node configuration to ${ENV_FILE}"
else
  echo "Keeping existing ${ENV_FILE}"
fi

cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Mailbox Tunnel Exit Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
ExecStart=${INSTALL_DIR}/mailbox-tunnel -mode node
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now mailbox-tunnel-node
sleep 2
echo ""
echo "=== Node Status ==="
curl -s http://127.0.0.1:7860 || journalctl -u mailbox-tunnel-node -n 20 --no-pager
