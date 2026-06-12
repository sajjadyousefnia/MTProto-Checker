# AGENTS.md — MTProto Deep Checker

## Quick start
```bash
npm install
node app.js          # Express server at localhost:3000, opens browser automatically
```

## Architecture
- **Single file app** — everything in `app.js`: Express server + embedded HTML/CSS/JS UI + GramJS MTProto client
- **No build step**, no codegen, no bundler, no config files
- **No tests** — `npm test` is a no-op placeholder
- **No CI/CD** — no workflows found
- **Bilingual** — Persian (fa, default, RTL) + English (en, LTR), stored in `localStorage`

## Key technical facts
| Fact | Detail |
|------|--------|
| Entrypoint | `app.js` (not `index.js` despite `package.json` `main` field) |
| Port | Hardcoded `3000` |
| API credentials | Hardcoded public Telegram test credentials (`API_ID=6`, `API_HASH=eb06d4...`) |
| MTProto lib | `telegram` (GramJS) v2.26.22 |
| Proxy check | Creates a real `TelegramClient` per proxy, calls `Api.help.GetConfig()` |
| Per-proxy timeout | 8s server-side + 10s client-side `AbortController` |
| Batch size | 10 concurrent checks |

## Gotchas
- `package.json` `main` points to `index.js` — ignore, real entry is `app.js`
- No TypeScript, no linting, no formatting config
- No `.env` — secrets are public test keys hardcoded in source
- `node_modules` gitignored; no lockfile to pin transitive deps (only `package-lock.json` exists)
- Only dependency: `telegram` (GramJS), `express`, `body-parser`, `open`
- Generate code that follows existing patterns: CommonJS (`require`/`module.exports`), no framework
