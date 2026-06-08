# SPEC — MTProto Checker Windows Executable

## §G — Goal

Bundle Node.js MTProto Checker into single Windows .exe, zero runtime deps.

## §C — Constraints

| C | Detail |
|---|--------|
| C1 | Output OS: Windows only (x64) |
| C2 | User must NOT need Node.js / npm pre-installed |
| C3 | Express static server on localhost:3000, auto-open browser |
| C4 | Native deps (bufferutil, utf-8-validate, websocket) must compile |  <!-- ? --> |
| C5 | ESM project (`"type": "module"`) — bundler must support |
| C6 | Single-file .exe preferred over folder |
| C7 | Must preserve all functionality: proxy check via GramJS |
| C8 | Build from Windows host only (native deps need Windows) |

## §I — Interfaces

| I | Surface | Detail |
|---|---------|--------|
| I1 | `app.js` | Express entrypoint, ES module |
| I2 | `public/` | Static assets (index.html, css/, js/) |
| I3 | `package.json` | Deps: express, telegram, open; type: module |
| I4 | `images/` | Readme assets (not needed at runtime) |
| I5 | output `.exe` | Single Windows portable binary |

## §V — Invariants

| V | Rule |
|---|------|
| V1 | .exe opens browser to localhost:3000 on first run |
| V2 | .exe serves public/ assets correctly |
| V3 | POST /check returns `{ok:bool, ping?:number}` same as node app.js |
| V4 | .exe exits cleanly on Ctrl+C (no orphan node process) |
| V5 | GramJS native deps (websocket, bufferutil) work inside bundle |
| V6 | Build reproducibility: same commit → same .exe hash |

## §T — Tasks

| T | Status | Description | Cites |
|---|--------|-------------|-------|
| T1 | x | research bundler for ESM + native deps | C5, C4 |
| T2 | x | spike: bundle with Node SEA | T1 |
| T3 | x | embed public/ assets in binary | I2 |
| T4 | x | build .exe on Windows | C8 |
| T5 | x | test standalone in isolated dir (no node_modules) | C2, V1-V4 |
| T6 | x | add npm scripts for build | |
| T7 | x | update README | |

## §B — Bugs

| id | date | cause | fix |
|----|------|-------|-----|
