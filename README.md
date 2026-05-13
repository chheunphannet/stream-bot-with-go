# Khdiamond Stream Bot

A Telegram bot written in Go that extracts embed stream URLs from `khdiamond.net`, supports movies and TV episodes, and stores usage stats in SQLite.

## Commands

- `/start`: show the welcome message.
- `/count_process`: show total users, total requests, and top users.

## Local Run

1. Create `.env`:

   ```bash
   cp .env.example .env
   nano .env
   ```

2. Set your Telegram bot token:

   ```env
   BOT_TOKEN=your_telegram_bot_token_here
   ```

3. Run:

   ```bash
   go run .
   ```

The bot keeps running until you stop it with `Ctrl+C`.

## VPS Deployment

Tested for Ubuntu-style VPS servers with systemd.

1. Install Go 1.21 or newer on the VPS.

2. Upload or clone this project, then create `.env`:

   ```bash
   cp .env.example .env
   nano .env
   ```

3. Deploy:

   ```bash
   chmod +x deploy.sh
   ./deploy.sh
   ```

By default the service is installed to `/opt/stream-bot` and runs as the user who runs `deploy.sh` with sudo access. To choose a different service user:

```bash
SERVICE_USER=ubuntu ./deploy.sh
```

If `stats.db` exists in the project folder, the deploy script copies it only when `/opt/stream-bot/stats.db` does not already exist. This protects production stats from being overwritten.

## Service Commands

```bash
sudo systemctl status stream-bot
sudo systemctl restart stream-bot
sudo systemctl stop stream-bot
sudo journalctl -u stream-bot -f
```

## Files

- `main.go`: bot source code.
- `stats.db`: SQLite stats database, created automatically at runtime.
- `.env`: production token file, not committed.
- `stream-bot.service`: systemd service template.
- `deploy.sh`: VPS build and install script.
