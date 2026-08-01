<!-- markdownlint-disable no-inline-html -->

<p align="center">
  <img src="icon.svg" width="128" height="128" alt="Telltale">
</p>

# Telltale

GitHub notifications for Telegram, done right.

- Issue & PR lifecycle events (opened, closed, merged, reopened, draft, ready for review)
- Bodies keep their formatting — headings, tables, task lists, alerts, `<details>`, code fences, footnotes and images all render natively
- Reply to notifications in Telegram → comments posted to GitHub via GitHub App
- Mention `#123` or a commit SHA in Telegram → bot sends a summary with PR preview build links

## Formatting

Notifications are sent as [rich messages](https://core.telegram.org/bots/api#rich-message-formatting-options)
(Bot API 10.1), whose Markdown follows GitHub Flavored Markdown closely enough
that issue and PR bodies pass through nearly untouched. Tables render as tables
rather than as flattened text or screenshots.

A few GitHub-isms have no Telegram equivalent and are rewritten before sending:
`#123` and commit SHAs become explicit links, since Telegram has no repo context
to resolve them against; `> [!WARNING]` alerts become labelled blockquotes; and
images that aren't alone on their own line collapse to links, because Telegram
renders media only as a standalone block.

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
GITHUB_DEFAULT_REPO=      # Default repo for autolink lookups (e.g. owner/repo)
GITHUB_ALLOWED_REPOS=     # Comma-separated owner/repo allowlist; empty allows all
PORT=8080                 # Server port (default: 8080)
DATABASE_PATH=telltale.db # SQLite path (default: telltale.db)
```

`GITHUB_ALLOWED_REPOS` is worth setting in production: a GitHub App has a single
App-level webhook URL, so every installation forwards its events here. Webhooks
from any other repo are dropped.

## Run

```sh
set -a && source .env && set +a && go run ./cmd/telltale
```

## Deploy

Push a semver tag to build and publish to GHCR:

```sh
git tag v0.1.0-alpha.1
git push origin v0.1.0-alpha.1
```
