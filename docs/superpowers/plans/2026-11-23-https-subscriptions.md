# Manual HTTPS Profile Subscriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Issue persistent client-specific subscription URLs and refresh managed profiles only on demand.

**Architecture:** The hub serves active profile exports on a separate TLS socket backed by the existing bbolt store. The client reuses the existing import parser and persists a URL beside its offline profile, while its TUI owns refresh and service restart.

**Tech Stack:** Go stdlib HTTPS, bbolt, existing Go TUI and Linux/Windows service controllers.

**Spec:** `docs/superpowers/specs/2026-11-23-https-subscriptions-design.md`.

## Global Constraints

- Public TCP 2228, configured hostname and PEM certificate/key; no admin routes or plaintext.
- One non-expiring secret URL per user and one raw `btawg://` line in the response.
- No background fetches or restart when a refresh fails or has no profile changes.
- Preserve local settings and enabled transports; support Linux and Windows managed clients.

## Review Focus

- Revoked and deleted users cannot fetch, even with formerly valid links.
- Rotated tokens stop working; issuance survives process restart.
- Pending hub settings never enter published profiles before activation.
- HTTPS redirects and invalid or oversized profiles leave settings untouched.
- Refresh changes only the imported profile, not local credentials or selected transport.

---

### Task 1: Serve and issue subscriptions

**Files:** `internal/store/{store,subscription}.go`, `internal/panel/{panel,subscription,templates}.go`, `cmd/btawg-server/{service,subscription,tui,main}.go` (under `btawg/`).

**Interfaces:** `(*Panel).SubscriptionHandler(host string) http.Handler` enables the authenticated panel controls and returns the isolated download handler. Public path `GET /sub/<token>`; response `text/plain; charset=utf-8`.

- [x] Add a TLS panel/endpoint lifecycle regression for issuance, refresh after active config changes, rotation, revoke, and deletion; observe failure before implementation.
- [x] Persist a random 32-byte secret per user in a separate bucket, delete it transactionally with the client, and validate against active enrollment on retrieval. Add panel issue/rotate controls and reuse active imports.
- [x] Wire optional host/cert/key settings through managed server JSON, CLI and setup TUI. Load PEM and bind TCP 2228 before readiness; include listener errors and shutdown.
- [x] Run focused hub, store and panel checks; exercise the real TLS socket and panel UI before committing.

### Task 2: Fetch and apply manually

**Files:** `internal/client/config.go`, `cmd/btawg-client/{main,tui,subscription,service_linux,service_windows}.go`, `btawg/README.md`.

**Interfaces:** `client.ManagedConfig.SubscriptionURL` (`subscription_url`) is stored with the offline `Import`. Fetch validates HTTPS, no redirects, and the existing profile contract. Existing managed install interface accepts file or hidden input; existing foreground modes stay static.

- [x] Add refresh regressions for invalid or unchanged response and valid changed profile preserving local settings; observe failure before implementation.
- [x] Store the selected source and resolved profile atomically, resolve before service disruption, and add the explicit refresh action. Use the platform service controller only for changed, validated updates.
- [x] Run focused client tests and a Windows cross-build, exercise the Linux TUI, and document setup/refresh/firewall and cert renewal.

## Verification evidence

- Isolated live hub: Chromium issued, copied, and rotated a link in the actual admin page. A trusted-certificate HTTPS request to TCP 2228 returned one import for the new URL and 404 for both the old URL and `/login`.
- Linux dashboard in an isolated PTY: manually refreshed an unchanged import, then an older profile. The latter applied the current profile with local settings intact and the inactive service left stopped. The service controller was isolated from the host.
- Windows client cross-builds for amd64 and arm64. No live Windows SCM test in this environment.
- Independent hub and client review reported no findings.
