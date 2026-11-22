# Windows x64 Client Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a native Windows x64 `btawg-client.exe` that behaves like the Linux client against the same Linux hub, using the same settings TUI and all current network features.

**Architecture:** Keep the TUI, managed client, wire protocols, settings schema, balancers and direct-peer policy shared. Windows uses Wintun for OS IPv4 traffic, isolated userspace overlay sockets for explicit proxy/control dials, and Windows-native service, IPC, installation and update backends.

**Tech Stack:** Go 1.27.4, existing `go-tui`, `amneziawg-go` and gVisor, signed amd64 Wintun, Windows IP Helper/Winsock/SCM/Event Log, GitHub Actions.

**Spec:** [Windows client parity design](../specs/2026-11-22-windows-client-parity-design.md).

## Global Constraints

- Windows 10 (22H2) and Windows 11 x64; no CGO. Keep both Linux binaries, existing Linux release artifact names and Linux behavior. Test changes from `btawg/` unless noted.
- Same `btawg://` imports and Linux hub, no Windows server. Preserve eight BitTorrent modes, native AWG/WG, DHT, per-mode SOCKS on 1080–1089, Failover/Round-robin/Adaptive on 1090–1092, DNS, egress and independent P2P pools and overrides.
- One Wintun with imported VPN IPv4, existing 1280 MTU and imported routes. No Windows VPN `/0` or host firewall automation. Overlapping LAN/VPN prefixes fail without modifying foreign state.
- Same TUI and defaults; elevated Windows Terminal for installation/management. Keep secrets out of process arguments, environment and logs. Running service is independent of its dashboard.
- Use anchored edits and the selective `commit` workflow; only commit reviewed task hunks. Follow TDD for implemented behavior; reuse existing assertions where they exercise the real platform. Never publish a debuggable or partial Windows client as parity.

## Review Focus

1. Existing LAN route contains a VPN subnet; abort without modifying it (Task 1).
2. Windows kernel and two proxy paths use the same VPN IP concurrently; never deliver replies to the wrong socket, including a late segment after close (Task 2).
3. Default gateway disappears while the service is up; no TUN default is activated and no external carrier loops into Wintun (Tasks 2–3).
4. An ordinary local process wins the pipe name before service start; neither send a control request to it nor write offline state because of it (Task 4).
5. Updater or host stops after staging the new DLL and EXE; restart activates one verified complete bundle and the same settings (Task 6).

## Ownership and Order

- TUN/IP Helper and live routing proof: `internal/tunsetup`, Task 1. The TUN/flow mux and Windows explicit-overlay sockets live in `internal/client`, Task 2; `internal/carrier` and peer underlay/portmap belong to Task 3.
- Service and IPC cross `cmd/btawg-client`, `internal/service` and `internal/client/daemon.go`; Task 4 owns those contracts. Task 5 keeps the UI composition shared and wires it to Task 4. Task 6 owns `internal/service/update*` and release asset construction. Task 7 owns integration harnesses, release gating and user docs.
- Start Task 1 on Windows before porting all callers. The TUN proof and Task 4's service/IPC package tests can be developed independently; build/run the real client for Task 4 after Tasks 2–3 and Task 6's common updater compile split. Finish Task 5 and the rest of Task 6 before the mixed-network release gate. Final Windows artifacts are enabled only in Task 7.

---

### Task 1: Bring up an owned Windows IPv4 TUN

**Files:** Move Linux `ip`/`sysctl` setup in `internal/tunsetup/tunsetup.go` and its privileged test into Linux-targeted files. Modify `internal/tunsetup/managed_linux.go` only to share prefix handling if necessary. Create `internal/tunsetup/managed_windows.go`, `internal/tunsetup/iphelper_windows.go`, `internal/tunsetup/managed_windows_test.go`. Do not include Linux hub/IP/NAT helpers in the Windows build.

**Interfaces:** `func OpenClientTUN(name string, addr netip.Prefix, routes []string, mtu int) (tun.Device, error)`; `func BindDevice(name string) func(string, string, syscall.RawConn) error` for overlay listeners; Windows-owned device closes its IP config and adapter. Implement the Windows side using Wintun's device LUID and `golang.org/x/sys/windows` IP Helper types, filling only missing calls in the focused Windows file.

- [ ] Write `TestWindowsClientTUNLifecycle` using real Wintun: assign `10.77.0.2/24`, route `10.78.0.0/24`, MTU 1280, no new `/0`; receive an IPv4 datagram from an ordinary socket and close. Assert both prefixes and adapter disappear from the live route/interface snapshot without changing the original default route. Write `TestWindowsClientTUNRejectsForeignState` using a temporary second adapter with a covering prefix; assert conflict names the prefix and adapter and snapshots are unchanged. Distinguish route-table query failures from absent routes.
- [ ] Run on elevated Windows with `wintun.dll` next to the test executable: `go test -c -o .\test-bin\tunsetup.test.exe ./internal/tunsetup` and `.\test-bin\tunsetup.test.exe -test.run '^TestWindowsClientTUN' -test.v`. Initially the Windows functions are missing; do not accept a driver-not-found result as the expected red state.
- [ ] Implement address and routes with IP Helper, selecting rows by LUID. Verify actual adapter ownership, not alias alone; reject competing setup, overlapping routes and foreign state before configuration. Roll back only owned entries on partial initialization, and serialize Wintun writes at the wrapped `tun.Device` boundary.
- [ ] Rerun the named Windows tests; require PASS with driver active. On Linux run `go test ./internal/tunsetup`; no behavior changes. Record results and commit `feat(client): add owned Windows IPv4 tunnel`.

### Task 2: Preserve proxy flow ownership without a host default route

**Files:** Modify `internal/client/client.go`, `assembly.go`, `shared_tun_linux.go` (move OS-independent dispatch into `shared_tun.go`), `tun_sockets_linux.go`; create `internal/client/tun_sockets_windows.go`, `overlay_netstack_windows.go`, `shared_tun_windows_test.go`. `internal/client/proxy.go`, `mesh.go` and `peer_probe.go` keep calling their common dial/listen boundaries.

The first Windows client-package test build also needs the OS-specific `TCPControl` from Task 3 and updater locking split from Task 6. Build those small seams before running Task 2's Windows tests. Keep Linux hub fixtures in the Linux-only test files that import `internal/server`, moving mixed-OS verification to Task 7; never exclude Linux tests from the Linux build.

**Interfaces:** `func (p *tunPath) proxySockets(c *Client, source netip.Addr) error`; `func (s *sharedTUN) newPath() *tunPath`; keep `Client.proxyTCP func(context.Context, netip.AddrPort) (net.Conn, error)` and `Client.proxyUDP func(context.Context) (net.PacketConn, error)`. Explicit userspace dials are not OS underlay dials; ordinary kernel packets and incoming egress/probe listeners still use Wintun.

- [ ] Add `TestWindowsSharedTUNProxyIsolation`: with two live paths interleave TCP and UDP to the same destination on both paths, plus actual OS traffic from `10.77.0.2`. Assert distinct wire ownership before the first SYN/datagram, responses reach only the requesting path, unsolicited inbound data still reaches the Wintun listener, and a delayed response after close is not sent to a new owner. Add `TestWindowsProxyWithMissingDefaultRoute` on an isolated test VM: remove only the fixture's default and prove an unbound ordinary socket is not redirected to Wintun. Include a competing exclusive wildcard bind, non-initial IPv4 fragments and ICMP errors in the isolation test.
- [ ] Run the Windows client tests with the same `go test -c`/adjacent DLL pattern and `-test.run '^TestWindows(SharedTUNProxyIsolation|ProxyWithMissingDefaultRoute)$'`. The isolated test network and real adapter are prerequisites.
- [ ] Reserve Windows TCP and UDP source ports before emitting traffic using ephemeral exclusive Winsock binds, and keep each port until its netstack endpoint ceases to own the wire flow. The pinned wrapper cannot dial TCP with a selected source port: use the existing gVisor `gonet.DialTCPWithBind` over the link endpoint rather than dialing then inspecting `LocalAddr`. Route incoming packets by flow, including fragments and quoted ICMP tuples; unrelated traffic must still reach Wintun. Ensure close and dial failure release ownership without misdelivery; do not use a globally fixed port range or rewrite the host routing table. Connect foreground `NewRealTUN` to the same single-path mechanism on Windows; preserve no-TUN and Linux behavior.
- [ ] Rerun the Windows isolation tests. Run `go test ./internal/client -run '^(TestSharedTUNRoutesProxyPackets|TestSharedTUNReadErrors|TestManagedBalancerPoolEditsDoNotMoveLiveSessions)$'` on Linux. Commit `feat(client): isolate Windows overlay sockets by flow`.

### Task 3: Keep the transport and peer underlay independent of Wintun

**Files:** `internal/client/client.go`, `peer_sockets.go`; `internal/carrier/tcp_windows.go`; `internal/portmap/route.go`, `route_linux.go`, `route_windows.go`, `route_windows_test.go`; use the existing underlay entry points in `internal/direct`, `internal/nativepeer`, `internal/dht` and vendored pure-Go µTP. `internal/server` remains Linux-only when using Linux TUN/routing functions.

**Interfaces:** `func TCPControl(network, address string, conn syscall.RawConn) error`; `func defaultRouter(ctx context.Context) (router, error)`; `func openPeerSockets(ctx context.Context, port uint16, autoMap, allowPermanentUPnP bool) (*peerSockets, error)`. Filter by actual VPN-adapter identity, not a hard-coded name or VPN address range.

- [ ] Write `TestWindowsPeerUnderlay` with a physical test link plus Wintun: assert the local P2P candidates omit Wintun, actual UDP ports on stream and native listeners are the ports offered for mapping, and test carrier/DHT traffic remains on the physical link. Add a gateway-loss case: no external packet is emitted on the Wintun path. In `TestWindowsDefaultRouter`, exercise multiple gateways with different route/interface metrics; assert the selected router, source and interface belong to the same physical link and no default yields an explicit error.
- [ ] Run the above Windows tests on the actual fixture before implementation. Add the Windows TCP socket buffer code without requesting Linux BBR. Select IPv4 gateway/ifindex/source via Windows route and adapter information; keep the existing PCP/NAT-PMP/IGD state machine, no TCP mapping. Use live physical route/source selection for underlay dials and observations; do not advertise the virtual adapter. Keep TCP and UDP ports/candidates independent, preserve currently permitted IPv6 candidates, and reuse the existing BitTorrent framing and fallback semantics unchanged.
- [ ] Rerun the named Windows tests; on Linux run `go test ./internal/{carrier,dht,direct,nativepeer,portmap}`. Exercise each mode in Task 7 rather than equating compilation with connectivity. Commit `feat(client): port underlay and peer discovery to Windows`.

### Task 4: Native service, permissions and authenticated local IPC

**Files:** Modify `cmd/btawg-client/main.go`, `internal/client/{config,control,daemon}.go`, `internal/service/{unit,files,notify}.go`; split existing Linux systemd operations into OS-targeted files; create `internal/service/{unit,files,notify}_windows.go`, `internal/client/control_windows.go`, `cmd/btawg-client/service_windows.go` and Windows service/control tests.

**Interfaces:** Keep `Unit.Install(extraArgs ...string) error`, `Unit.Control(action string) error`, `Unit.State() (string, bool, error)`, `Unit.Uninstall() error`, `service.WriteFile(path string, data []byte, mode os.FileMode) error` and `client.RequestControl(req ControlRequest) (ControlResponse, bool, error)`. Add `listenControl(path string) (net.Listener, error)` and `dialControl(path string) (net.Conn, error)` behind the daemon and request encoders. Use internal `runManagedAt(ctx context.Context, cfg ManagedConfig, cfgPath, socketPath string, realTUN bool, tunName string, portBase int, ready func() error) error`, invoked at the current `NotifyReady` boundary, after local initialization but before WAN connectivity; keep the Linux caller using its current systemd notification.

- [ ] Add `TestWindowsSCMLifecycle` on elevated Windows: status progresses from absent to started, toggling autostart to manual keeps manual Start available, Stop cleans up, Uninstall leaves the config and executable. Add `TestWindowsControlRejectsWrongServer` with an unrelated local listener holding the name: genuine service startup fails, client sends no request to it, and secret file is unchanged. Add `TestWindowsControlOfflineStartRace` to prove the live configuration observes the last completed offline write, and test a non-admin token cannot read config or mutate the running client.
- [ ] Run the new Windows tests; require genuine SCM/pipe behavior, not fake status. Implement service management with `x/sys/windows/svc` and `svc/mgr`, filesystem locations from Windows known folders, secret-file ACLs on creation and replacement, local first-instance pipe with Administrators/SYSTEM ACL and remote rejection. Validate server PID against SCM before JSON is written, and deny delegation/impersonation on the client pipe handle. Coordinate start/config fallback with a secured cross-process lock. Preserve bounded reads/cancel and close active IPC on service stop. Report SCM states and Event Log entries, handle privileged lifecycle without arguments containing secrets, and retain original Linux systemd hooks.
- [ ] Run the Windows lifecycle, unauthorized pipe and start-race scenarios; `go test ./internal/service ./internal/client ./cmd/btawg-client` on Linux. Commit `feat(client): manage a protected native Windows service`.

### Task 5: Use the same dashboard and CLI on Windows

**Files:** Modify `cmd/btawg-client/{main,tui,peer_tui}.go` only at platform seams; `internal/service/tui.go`; add native `internal/service/console_windows.go` for admin detection, log retrieval and selection support as needed. Reuse existing dashboard tests.

**Interfaces:** Keep `runTUI() error`, `clientActions(string) ([]service.Action, error)`, `service.TUI(menu Menu) error`, `Unit.logs() (*Screen, error)` and status vocabulary consumed by the current UI. Only platform help or error copy differs.

- [ ] Reuse `TestPeerScreensPreserveLatestIndependentSettings` and `TestPeerOverrideScreensEditAndRestoreInheritance` on Windows. In an elevated Windows Terminal session prove `Install service` → hidden `VPN import` → live dashboard, toggle a fixed port with Enter/Space, edit and reorder each pool, choose an egress route, show/rotate credentials, use Ctrl+T and copy, edit P2P defaults and per-peer overrides, view Event Log from `View service logs`, and Quit without stopping service. Run the foreground and `install -import-file` CLI paths as well. Record actual TUI state rather than constructing a lookalike screen in tests.
- [ ] Make console and service operations platform-specific only as necessary. Keep existing forms, keys and order, including credential concealment and copy/selection; use Windows Console Host fallback only where it preserves expected behavior, without promising unsupported terminal hosts.
- [ ] Run `go test ./cmd/btawg-client -run '^TestPeer'` on both platforms and repeat the actual dashboard/CLI session after wiring. Commit `feat(client): run the shared settings TUI on Windows`.

### Task 6: Update verified Windows bundles without partial activation

**Files:** Modify `internal/service/update.go`; create `internal/service/update_windows.go`, `update_linux.go`, `update_windows_test.go`; `cmd/btawg-client/main.go` for the authenticated update handoff; `btawg/scripts/package-windows.ps1` for the exact ZIP layout.

**Interfaces:** Keep `Unit.updateFrom(ctx context.Context, client *http.Client, progress func(string)) (updateResult, error)` and `Unit.applyRelease(ctx context.Context, client *http.Client, tag string, progress func(string)) error`. Windows downloads `btawg-client-windows-amd64.zip` and validates its entry in `SHA256SUMS`; Windows package contains `btawg-client.exe`, `wintun.dll`, `Wintun-LICENSE.txt`. Pin the official Wintun 0.14.1 source ZIP digest `07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51` from upstream; use its amd64 DLL and accompanying license.

- [ ] Add `TestWindowsBundleUpdateTransaction`: from an installed older build, wrong checksum leaves service, configuration and paths unchanged; valid update replaces the pair and preserves running/stopped and automatic/manual settings; failed service readiness restores old pair and state; interruption before and after activation recovers to a matching verified pair, never mixed. Use controlled release HTTP fixtures as in `TestUpdateFromRelease`; production source remains unchanged. Check the old-TUI-to-new-launch result is truthful if the applier fails.
- [ ] Run the named Windows scenario against the previous/next tagged-identity fixtures before implementing the transaction. Stage complete protected bundle directories; activate with one durable selection change, keep the old bundle until readiness commits, and recover from an interrupted activation. Run the applier from a distinct process if needed to release the interactive image. Reject missing, extra or unsafe ZIP entries, preserve the upstream DLL signature, and use the existing direct stable-release lookups and bounded checksum verification. No legacy Linux asset format changes.
- [ ] Rerun `TestWindowsBundleUpdateTransaction`; on Linux run `go test ./internal/service -run '^TestUpdateFromRelease$'`. On Linux build `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/btawg-client`; this is not Windows runtime evidence. Commit `feat(client): install and roll back verified Windows bundles`.

### Task 7: Prove full parity and gate the release

**Files:** Modify `.github/workflows/{ci,release}.yml`, `btawg/README.md`; create `btawg/scripts/windows-parity.ps1`, `btawg/scripts/windows-parity-hub.sh` as repeatable verification harnesses and `btawg/internal/client/windows_parity_test.go` for privileged Windows scenarios not already exercised. Use dedicated test users, separate Windows and Linux peers, a Linux hub on a reachable underlay, local TCP/UDP echo targets and a controlled NAT/router environment.

**Interfaces:** `windows-parity.ps1 -Bundle <ZIP> -Fixture <file> -ExpectedSha256 <sha256>`; fixture supplies test-only import files, Linux peer VPN IP, underlay endpoint/echo addresses and NAT fixture metadata. Never echo imports or publish harness-generated keys/credentials. The script reports pass/fail per mode and exits nonzero on missing or failing observations. Tag build exports `btawg-client-windows-amd64.zip` and checksum; publication downloads that unmodified artifact after the configured Windows gate.

- [ ] Extend interoperability smoke from `TestMixedTransportStableAddress`, `TestManagedDirectTUNWithHubRevocation` and `TestManagedEgressCascadeAndLiveRevocation`. Exercise TCP/UDP through **each** of 1080–1089 against Linux, DHT on each carrier mode using reachable peers, and 1090–1092 under path loss and recovery. Probe correct DNS and selected exit, reverse egress serving, rejection on revoked ACL, and no host default-route change. Test direct P2P in both initiation directions with all ten eligible modes and the independent pools, per-peer overrides, manual candidates, PCP/NAT-PMP/UPnP, gated relay and live revocation. Check stable VPN-IP and real app-to-app TCP/UDP; use packet/endpoint observation to distinguish direct vs relay, not just successful traffic. No skips on the dedicated stand for these acceptance cases.
- [ ] Add an elevated Windows x64 CI job that at least actually loads Wintun and runs privileged suite where the fixture is present. Configure a protected Windows release gate; build once, run the exact ZIP on Windows 10 (22H2) and Windows 11 x64 with a Linux hub and peer, record SHA-256 and real TUI/service/reboot/update results, then publish those same bytes. Do not let an unconfigured approval environment silently bypass the required Windows proof. Validate Linux release names and checksums unchanged; `--version` reports the tag on both OSes.
- [ ] Update README with ZIP contents, elevated Windows Terminal setup, ordinary firewall responsibility and P2P exceptions, overlapping-route error, CLI, removal and updating. Document expected verification commands/run artifacts. Do not present an unverified build as a release.
- [ ] Run Windows elevated component tests; run the complete mixed-network harness on the tagged ZIP, including the actual Windows Terminal checklist from Task 5. On Linux run the existing CI verification commands and release build/version smoke. Record actual output and digests; report any unavailable Windows environment as an outstanding gate, not a pass. Commit `ci: verify and release the Windows x64 client` only once the corresponding changes have been verified.
