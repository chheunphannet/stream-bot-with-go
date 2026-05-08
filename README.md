# Khdiamond Stream Bot

A Telegram bot written in Go that extracts embed stream URLs from `khdiamond.net`. It supports both movies and TV shows, tracks usage statistics, and includes deployment scripts for systemd.

## Features

- **Stream Extraction**: Automatically fetches embed URLs from `khdiamond.net`.
- **Media Support**: Handles both Movie and TV Show content.
- **Usage Statistics**: Tracks total requests and identifies top users (saved in `stats.json`).
- **Asynchronous Processing**: Uses Go routines for non-blocking update handling.
- **Production Ready**: Includes a systemd service file and a deployment script for easy VPS setup.

## Commands

- `/start`: Displays the welcome message.
- `/count_process`: Shows usage statistics (total users, total requests, and top 5 users).

## Getting Started

### Prerequisites

- Go 1.21 or higher
- A Telegram Bot Token (obtainable from [@BotFather](https://t.me/BotFather))

### Setup

1.  **Clone the repository**:
    ```bash
    git clone https://github.com/chheunphannet/stream-bot-with-go.git
    cd stream-bot-with-go
    ```

2.  **Configure environment variables**:
    Create a `.env` file in the root directory and add your bot token:
    ```env
    BOT_TOKEN=your_telegram_bot_token_here
    ```

3.  **Install dependencies**:
    ```bash
    go mod tidy
    ```

4.  **Run the bot**:
    ```bash
    go run main.go
    ```

## Deployment

To deploy the bot on a Linux VPS (e.g., Ubuntu):

1.  Edit `deploy.sh` and `stream-bot.service` if you need to change paths or the user.
2.  Run the deployment script:
    ```bash
    chmod +x deploy.sh
    ./deploy.sh
    ```
3.  Check the service status:
    ```bash
    sudo systemctl status stream-bot
    ```
4.  View logs:
    ```bash
    sudo journalctl -u stream-bot -f
    ```

## License

This project is for educational purposes. Ensure you comply with the terms of service of the target website.
