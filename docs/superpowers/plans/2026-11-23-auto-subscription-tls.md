# Automatic Subscription TLS Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans or superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** An upgraded managed VPS issues existing per-client HTTPS links without operator-provided PEM files.

**Architecture:** Use Go's ACME client with the Let's Encrypt `shortlived` profile, IP identifiers, and a temporary standalone HTTP-01 listener on TCP 80. Keep certificate/account keys in the protected hub bbolt store. Serve current certificates on TCP 2228, replacing them atomically before expiry. A failed listener or issuance never blocks hub startup; the import page describes the current state. Explicit PEM files retain precedence. Foreground runs opt in with `-subscription-auto`; managed runs enable it by default. The only public HTTP handler serves the active ACME challenge. No firewall automation, TLS verification bypass, or VPN client changes.

**Tech Stack:** `golang.org/x/crypto/acme`, `net/http`, `crypto/tls`, bbolt.

**Spec:** Approved in conversation: IP-based HTTPS on port 2228, automatic issuance and renewal via opened port 80, manual profile refresh remains unchanged.

## Review Focus

- Restart uses a valid persisted certificate without requiring TCP 80 or an external network call.
- Renewal switches only the handshake certificate; a failed renewal keeps serving an unexpired certificate.
- Pending/error states show in the panel without exposing secrets or blocking VPN, and link creation waits for valid TLS.
- Challenge responses are only served for the active token; no profiles or admin endpoints on port 80.

## Task 1: Issue and maintain the IP certificate

- [x] Add protocol fixture exercising IP order, `shortlived`, HTTP-01 path, CSR, certificate persistence, failure, and replacement.
- [x] Implement certificate lifecycle in `cmd/btawg-server/subscription_acme.go`, and scoped persistent state in `internal/store/subscription_tls.go`.
- [x] Use supported ACME profile APIs from `golang.org/x/crypto` and verify focused tests.

## Task 2: Make managed boot and the panel reflect availability

- [x] Exercise pending-to-ready and failure UI states in the import panel; preserve the static import.
- [x] Wire nonblocking startup and shutdown in `cmd/btawg-server/service.go` and `subscription.go`. Apply manual PEM precedence and actual certificate readiness.
- [x] Verify focused packages and a live browser against a disposable hub, document ports, renewal, and the manual override.

## Verification evidence

- A local ACME protocol fixture exercised `shortlived` IP ordering, the live HTTP-01 responder, and finalization with an IP SAN CSR.
- A live TLS test served a valid certificate through a failed renewal, hot-swapped to the new certificate, and restored it from a reopened database.
- A disposable hub restored a cached certificate with no PEM settings and no TCP 80 listener. Chromium issued an IP subscription link; curl validated the IP SAN against the test certificate and received one profile. A second hub with a private IPv4 kept serving its panel and exposed the error without offering a broken link.
- Production Let's Encrypt issuance requires a publicly reachable VPS and was not attempted in the local smoke.
