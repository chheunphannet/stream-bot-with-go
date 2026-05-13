#!/usr/bin/env bash
# Build and install the bot on a Linux VPS with systemd.

set -euo pipefail

APP_NAME="stream-bot"
BOT_DIR="${BOT_DIR:-/opt/stream-bot}"
SERVICE_USER="${SERVICE_USER:-${SUDO_USER:-$USER}}"
SERVICE_GROUP="${SERVICE_GROUP:-}"
SERVICE_FILE="/etc/systemd/system/${APP_NAME}.service"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is not installed. Install Go 1.21 or newer first."
  exit 1
fi

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  echo "Service user '$SERVICE_USER' does not exist."
  exit 1
fi

if [ -z "$SERVICE_GROUP" ]; then
  SERVICE_GROUP="$(id -gn "$SERVICE_USER")"
fi

if [ ! -f ".env" ] && [ ! -f "${BOT_DIR}/.env" ]; then
  echo "Missing .env. Create it from .env.example and set BOT_TOKEN."
  exit 1
fi

echo "[1/5] Preparing dependencies..."
go mod tidy

echo "[2/5] Building ${APP_NAME}..."
go build -trimpath -ldflags="-s -w" -o "${APP_NAME}" .

echo "[3/5] Installing files to ${BOT_DIR}..."
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$BOT_DIR"
sudo install -m 0755 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "${APP_NAME}" "${BOT_DIR}/${APP_NAME}"

if [ ! -f "${BOT_DIR}/.env" ]; then
  sudo install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_GROUP" .env "${BOT_DIR}/.env"
  echo "Copied .env to ${BOT_DIR}/.env"
fi

if [ -f "stats.db" ] && [ ! -f "${BOT_DIR}/stats.db" ]; then
  sudo install -m 0644 -o "$SERVICE_USER" -g "$SERVICE_GROUP" stats.db "${BOT_DIR}/stats.db"
  echo "Copied existing stats.db to ${BOT_DIR}/stats.db"
fi

sudo chown -R "$SERVICE_USER:$SERVICE_GROUP" "$BOT_DIR"

echo "[4/5] Installing systemd service..."
tmp_service="$(mktemp)"
sed "s|^User=.*|User=${SERVICE_USER}|" stream-bot.service > "$tmp_service"
sudo install -m 0644 "$tmp_service" "$SERVICE_FILE"
rm -f "$tmp_service"

echo "[5/5] Starting service..."
sudo systemctl daemon-reload
sudo systemctl enable "$APP_NAME"
sudo systemctl restart "$APP_NAME"

echo "Done."
echo "Status: sudo systemctl status ${APP_NAME}"
echo "Logs:   sudo journalctl -u ${APP_NAME} -f"
