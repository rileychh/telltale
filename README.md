<p align="center">
  <img src="icon.png" width="128" height="128" alt="Telltale">
</p>

# Telltale

GitHub notifications for Telegram, done right.

- Issue & PR lifecycle events (opened, closed, merged, reopened, draft, ready for review)
- Markdown rendered as native Telegram formatting (bold, code, blockquotes, autolinks)
- Reply to notifications in Telegram → comments posted to GitHub via GitHub App

## Setup

1. Create a [Telegram bot](https://t.me/BotFather) and add it to your group
2. Create a [GitHub App](https://github.com/settings/apps/new) with Issues and Pull Requests read/write permissions
3. Add a webhook to your repo pointing to `https://your-host/webhook/github`
4. Set the Telegram webhook to `https://your-host/webhook/telegram`

## Configuration

```sh
TELEGRAM_TOKEN=           # Bot token from BotFather
TELEGRAM_CHAT_ID=         # Target group chat ID
GITHUB_WEBHOOK_SECRET=    # Secret for validating GitHub webhooks
GITHUB_APP_ID=            # GitHub App ID
GITHUB_PRIVATE_KEY_PATH=  # Path to GitHub App private key (.pem)
PORT=8080                 # Server port (default: 8080)
```

## Run

```sh
# Local
set -a && source .env && set +a && go run ./cmd/telltale

# Docker
docker compose up
```

## Deploy

Push a semver tag to build and publish to GHCR:

```sh
git tag v0.1.0-alpha.1
git push origin v0.1.0-alpha.1
```
