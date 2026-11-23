# Manual HTTPS profile subscriptions

Give each enrolled user one stable URL that retrieves the current `btawg://` profile even when the VPN cannot connect. Linux and Windows managed clients fetch only on explicit installation, replacement, or **Refresh subscription**. No startup or periodic fetch, native third-party formats, quota, or expiry. Mesh control remains unchanged.

## Approaches

- Selected: a dedicated HTTPS listener in `btawg-server` on TCP 2228, using a configured public hostname and PEM cert/key files. No second process and no public admin routes.
- A reverse proxy would manage certificate termination but introduces another required process and configuration. A tunnel-only endpoint cannot repair a broken tunnel. Neither is required.

## Contract

- All three subscription settings are required to enable the listener. None leaves it disabled. Invalid settings, PEM files or socket binding fail startup explicitly; HTTP is not an alternative. The public URL is derived from configured hostname, not the incoming `Host` header.
- The authenticated panel import page issues, copies, and rotates each user's non-expiring bearer URL. A URL is a secret because the profile contains a private key. A rotation invalidates the old URL without touching VPN access. Revoked and deleted clients cannot download a profile. A separate bbolt bucket keeps subscription writes independent of profile/policy churn.
- `GET /sub/<token>` on port 2228 returns exactly one plain-text `btawg://` and a newline, with `Cache-Control: no-store`. Use the existing active-profile builder. Invalid, unknown, rotated, revoked and deleted credentials do not expose client data. Do not log URLs, response content, or tokens.
- A subscription URL may replace the static import at managed setup. The client validates HTTPS with system roots, follows no redirects, bounds the response and fetch time, stores the URL together with the downloaded import in the protected settings file, and keeps using that import offline. Static imports remain supported. On refresh, a valid changed profile updates only the saved import; enabled transports and local settings remain untouched. An unchanged or failed fetch does not write or restart. Only a running service restarts after a change. A stopped service remains stopped.
