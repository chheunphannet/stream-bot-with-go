#!/bin/bash

# Exit on error
set -e

echo "🚀 Starting deployment..."

# 1. Pull latest code
echo "📥 Fetching latest code from feat/curl-impersonate..."
git fetch origin
git reset --hard origin/feat/curl-impersonate
git clean -fd

# 2. Update .env if needed
if [ ! -f .env ]; then
    echo "📄 Creating .env from .env.example..."
    cp .env.example .env
    echo "⚠️  Please edit .env and add your BOT_TOKEN!"
fi

# 3. Clean and Build
echo "🏗️  Building fresh binary..."
rm -f stream-bot
go clean -cache
go build -o stream-bot main.go

# 4. Restart Service
echo "🔄 Restarting stream-bot service..."
sudo systemctl daemon-reload
sudo systemctl restart stream-bot || echo "⚠️ Service not started yet or failed. Please check with: journalctl -u stream-bot -f"

echo "✅ Deployment successful!"
echo "📡 Check logs with: journalctl -u stream-bot -f"
