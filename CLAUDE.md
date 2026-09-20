# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

PanDrive Lite — Google Drive multi-account gateway. Go backend + SQLite, React (Preact-compat) frontend. Runtime target: 1 vCPU / 1 GB RAM VPS, one Go binary, SQLite WAL, no Docker/MySQL/S3.

## Commands

Backend (all inside `backend-go/`):

```bash
go test ./...                  # all tests
go test -run TestName .        # single test
go build -o ../bin/pandrive .    # build
go run .                       # run (needs env vars below)
```

Required env vars: `APP_PORT`, `FRONTEND_URL`, `JWT_ACCESS_SECRET`, `TOKEN_ENCRYPTION_KEY`, `DATABASE_URL` (e.g. `file:data/pandrive.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`).

Frontend (inside `frontend/`): `npm run dev` (Vite, port 5173), `npm run build` (`tsc && vite build`).

## Architecture

**Backend is one file**: `backend-go/main.go` (~1600 lines) contains config, SQLite migrations, all HTTP handlers, Google OAuth, resumable uploads, downloads. Stdlib `net/http` only — no router framework, routes registered on `http.ServeMux` in `App.Router()`. All handlers are methods on `App` (holds DB + Config). Tests live next to it as `*_test.go` with helpers in `test_helpers_test.go`.

Key flows:

- **Auth**: JWT access tokens; refresh tokens hashed in SQLite `sessions` table. `requireAuth` middleware injects user into context.
- **Secrets**: Google OAuth client secret and account tokens stored AES-GCM encrypted (`encrypt`/`decrypt`, key from `TOKEN_ENCRYPTION_KEY`).
- **OAuth**: `googleConnectURL` → `googleCallback`; state is hashed, 10-min expiry.
- **Uploads**: resumable sessions proxy straight to Google Drive upload URLs — chunks never hit disk (`initResumableUpload` → `resumableChunk`, offset tracked via Google `Range` header in `nextUploadOffset`).
- **Account routing**: `selectAccountForUpload` picks Google account by policy (`getRoutingPolicy`/`updateRoutingPolicy`) since files are spread across multiple accounts.
- **Downloads**: `downloadFile` proxies range byte streams from Google Drive.
- **Bootstrap**: `POST /auth/register` works only for first user (`ensureInitialAdmin`); disabled after.

**Frontend**: React 19 with **Preact compat aliases** in `vite.config.ts` (`react` → `preact/compat`) — write code as React; bundling swaps it. Path alias `@` → `src/`. Tailwind v4 via `@tailwindcss/postcss`. PWA via `vite-plugin-pwa`; workbox `navigateFallbackDenylist` exempts API paths. API client in `src/lib/api.ts`; upload state in `src/context/UploadContext.tsx`.

**Legacy**: old TypeScript/Prisma backend under `backend/` is deleted in git; migration to Go complete.

## Conventions

- Backend config only via env vars (`loadConfig`); no config files.
- DB schema created idempotently in `App.migrate()` — add new tables/columns there.
- SQLite with WAL + busy_timeout; single-writer assumptions.
- Google Drive API calls made with plain `net/http` + `oauth2` — no Google client SDK.
