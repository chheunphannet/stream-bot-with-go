#!/bin/bash
# deploy.sh — build and install on your VPS

set -e

BOT_DIR="/opt/stream-bot"

echo "→ Building..."
go mod tidy
go build -o stream-bot .

echo "→ Installing to $BOT_DIR..."
sudo mkdir -p $BOT_DIR
sudo cp stream-bot $BOT_DIR/
sudo cp stream-bot.service /etc/systemd/system/

# Copy .env if it doesn't exist yet
if [ ! -f "$BOT_DIR/.env" ]; then
  sudo cp .env $BOT_DIR/.env
  echo "→ Copied .env"
fi

echo "→ Enabling service..."
sudo systemctl daemon-reload
sudo systemctl enable stream-bot
sudo systemctl restart stream-bot

echo "✓ Done! Check status: sudo systemctl status stream-bot"
echo "✓ View logs:          sudo journalctl -u stream-bot -f"
