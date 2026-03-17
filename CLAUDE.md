# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Run locally (load env first)
set -a && source .env && set +a && go run ./cmd/telltale

# Build binary
go build ./cmd/telltale

# Docker
docker compose up --build
```

There are no tests or linter configured in this project.

## Environment

Configuration via environment variables (see `.env.example`):
- `TELEGRAM_TOKEN`, `TELEGRAM_CHAT_ID` — Telegram bot credentials
- `GITHUB_WEBHOOK_SECRET` — HMAC-SHA256 webhook validation secret
- `GITHUB_APP_ID`, `GITHUB_PRIVATE_KEY_PATH` — GitHub App auth
- `PORT` (default 8080), `DATABASE_PATH` (default telltale.db)

## Architecture

Telltale is a GitHub↔Telegram bridge bot. It receives GitHub webhooks, formats them as Telegram messages, and routes Telegram replies back as GitHub comments.

### Packages

- **`cmd/telltale`** — Entry point. Loads config, initializes components, sets up HTTP routes (`POST /webhook/github`, `GET /health`, dynamic Telegram webhook), handles graceful shutdown.
- **`internal/github`** — GitHub webhook handler and API client. Validates webhooks with HMAC-SHA256, processes issue/PR/comment/review events, converts markdown to Telegram HTML. Authenticates as a GitHub App via `ghinstallation`.
- **`internal/telegram`** — Telegram bot. Sends HTML notifications, handles reply-to-comment flow (looks up GitHub context, posts comment, saves mapping, reacts with 👀).
- **`internal/store`** — SQLite store mapping Telegram message IDs → GitHub context (repo, issue number, comment ID). Enables stateful reply routing.

### Key Flow: Reply Routing

1. GitHub webhook → handler formats notification → Telegram message sent → mapping saved to SQLite
2. User replies to Telegram message → handler looks up mapping → fetches quote context from GitHub → posts comment → saves new mapping → adds reaction

### Markdown/HTML Conversion (`internal/github/html.go`)

Regex-based converter from GitHub markdown to Telegram-compatible HTML. Uses a placeholder system to protect code blocks, links, and blockquotes during transformation. Handles autolinks (#refs, commit SHAs), checkboxes, and formatting.
