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

## Git

Commit directly to `main`. This repo has no branch or PR workflow.

## Verifying Message Rendering

Rich Markdown is parsed server-side, so a Telegram client is the only real check
on how a notification looks. See `CLAUDE.local.md` for the scratch bot and the
read-back procedure. Never send test traffic to the production chat in `.env`.

## Environment

Configuration via environment variables (see `.env.example`):

- `TELEGRAM_TOKEN`, `TELEGRAM_CHAT_ID` — Telegram bot credentials
- `GITHUB_WEBHOOK_SECRET` — HMAC-SHA256 webhook validation secret
- `GITHUB_APP_ID`, `GITHUB_PRIVATE_KEY_PATH` — GitHub App auth
- `GITHUB_DEFAULT_REPO` — Default repo for autolink lookups (e.g. `owner/repo`), optional
- `GITHUB_ALLOWED_REPOS` — Comma-separated allowlist of `owner/repo` values; webhooks from any other repo are dropped (logged and 200-acked). Empty = allow all. Recommended in production since GitHub Apps have a single App-level webhook URL, so any installation forwards events here.
- `PORT` (default 8080), `DATABASE_PATH` (default telltale.db)

## Architecture

Telltale is a GitHub↔Telegram bridge bot. It receives GitHub webhooks, formats them as Telegram messages, and routes Telegram replies back as GitHub comments.

### Packages

- **`cmd/telltale`** — Entry point. Loads config, initializes components, sets up HTTP routes (`POST /webhook/github`, `GET /health`, dynamic Telegram webhook), handles graceful shutdown.
- **`internal/github`** — GitHub webhook handler and API client. Validates webhooks with HMAC-SHA256, processes issue/PR/comment/review events, prepares markdown for Telegram. Authenticates as a GitHub App via `ghinstallation`.
- **`internal/telegram`** — Telegram bot. Sends notifications as rich messages, handles reply-to-comment flow (looks up GitHub context, posts comment, saves mapping, reacts with 👀). Also handles autolink previews for `#N` and commit SHA references.
- **`internal/store`** — SQLite store mapping Telegram message IDs → GitHub context (repo, issue number, comment ID). Enables stateful reply routing.

### Key Flow: Reply Routing

1. GitHub webhook → handler formats notification → Telegram message sent → mapping saved to SQLite
2. User replies to Telegram message → handler looks up mapping → fetches quote context from GitHub → posts comment → saves new mapping → adds reaction

### Key Flow: Autolink Previews

1. User sends message containing `#N` or commit SHA in Telegram
2. Bot looks up the issue/PR/commit via GitHub API
3. For PRs, bot finds the `<!-- pr-preview-comment -->` comment to extract build number and install links
4. Bot sends a formatted summary (e.g. `#205 fix: title\nBuild 680 ⋅ Install on Android ⋅ iOS skipped`)

### Message Formatting

Notifications are sent with `sendRichMessage` ([Rich Messages](https://core.telegram.org/bots/api#rich-message-formatting-options), Bot API 10.1). Rich Markdown is GitHub Flavored Markdown where possible, so GitHub bodies pass through nearly untouched — headings, tables, ordered and task lists, dividers, `<details>`, code fences and footnotes all render natively. Limits: 32768 characters, 500 blocks, 50 media, 20 table columns.

`internal/github/html.go` is only a preprocessor (`prepareMarkdown`), handling what Telegram can't infer:

- **GitHub autolinks** — `#N` and commit SHAs become explicit Markdown links, since Telegram has no repo context and a bare `#N` would be detected as a hashtag. Code spans, existing links and bare URLs are placeholder-protected first, so hex in a URL is never mistaken for a SHA.
- **GitHub alerts** — `> [!WARNING]` is a GitHub extension, not core GFM, so the marker would show through as literal text. It becomes an emoji-and-bold title line in the same blockquote, followed by an empty quote line so the title doesn't run into a prose body. The blockquote is kept rather than swapped for an `<aside>` pull quote: `<aside>` looks closer to a callout but doesn't parse Markdown inside, and alert bodies routinely carry links and code. Only the five GitHub types are recognised, and only on the quote's first line, so anything GitHub renders literally stays literal here too.
- **Images** — Telegram renders media only as a standalone block, so an image is left as `![](url)` only when it is alone on its line and isn't an SVG. Anything inline or wrapped in a link (badges, typically) collapses to a plain link.
- **Angle brackets** — Telegram silently discards tags it doesn't recognise, so bare `<T>` or `List<String>` in prose would vanish from the message. Every `<` that doesn't open a supported tag is escaped to `&lt;`; HTML comments are dropped outright, matching how GitHub renders them. Code spans and fences are protected beforehand, so generics inside them are untouched.
- **Block HTML** — Telegram does not parse Markdown inside block tags (`<table>`, `<ul>`, `<blockquote>`, …), with `<details>` the notable exception. Inside those blocks an image is demoted to an HTML `<a>` anchor rather than a Markdown link, which would otherwise render as literal `[text](url)` *and* break the surrounding table.

HTML tables now pass through and render natively (`colspan`, `rowspan`, `align`), so unlike the old converter they are no longer stripped to bare text. Markdown table cells support inline formatting only — an image in a cell becomes a link, since media can't live inside a table.

`escapeMarkdown` escapes interpolated GitHub data (logins, titles) for message headers. Autolink previews deliberately stay on regular messages with `parse_mode=HTML` — Telegram recommends those for short text, and they keep features like partial quotes.

If a rich send fails (malformed markdown, block limit, rejected media URL), `Handler.send` falls back to a plain HTML text message so the notification still arrives.
