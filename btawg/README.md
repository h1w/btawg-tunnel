# btawg — routed IPv4 and SOCKS5 over BitTorrent

Two Linux binaries: `btawg-server` (VPS hub) and `btawg-client` (device node).
Use AmneziaWG or mutual TLS 1.3 inside BitTorrent, or native AmneziaWG
and WireGuard over UDP. Clients use an imported hub endpoint by default;
BitTorrent modes also support DHT discovery. The hub's web panel manages
VPN addresses, groups and `btawg://` imports.

Built on the validated transport: extended (BEP 10) over µTP no-Nagle —
**+0.07 ms p95 vs bare AWG**, classified as `BitTorrent` by nDPI 6.1.0 with
no risk flags (`../experiments/awg-bittorrent/FINDINGS.md`).

## Build

```sh
cd btawg
CGO_ENABLED=0 go build -o bin/btawg-server ./cmd/btawg-server
CGO_ENABLED=0 go build -o bin/btawg-client ./cmd/btawg-client
go test ./...
```

`third_party/go-utp` is a vendored copy of anacrolix/go-utp with one patch:
`purego.MatrixDisableNagle` bypasses the µTP Nagle hold (the validated
latency win; see the benchmark for why this matters).

## Quickstart (VPS + one client, from zero)

If installing from a release, use the filename for your architecture from
[Release downloads](#release-downloads) in place of `./btawg-server` or
`./btawg-client` in the first-run commands below. The installer uses the
unsuffixed names for the installed commands and services.

### 1. Hub on a public-IP VPS

```sh
sudo ./btawg-server
```

Choose **Install service**. The installer copies the binary to `/usr/local/bin`,
writes and enables `btawg-server.service`, and starts it. Save the initial
`admin` password shown in the TUI; press Enter to acknowledge it, or Esc to
leave it available under **Web panel and initial login**. Do not delete the
database to recover a password: it contains the hub and client identities.

To copy a login or password, select its value on the **Web panel and initial
login** screen or the client's **Show SOCKS5 credentials** screen. Each value
is on its own line, and these screens leave the mouse to the terminal. On
other screens, **Ctrl+T** switches between terminal selection and menu
clicks. Use ↑/↓ or PgUp/PgDn to scroll a document before selecting text.

- **Local terminal or SSH:** drag to select, then use the local terminal's
  Copy action (often **Ctrl+Shift+C**). In PuTTY on Windows, dragging and
  releasing copies the selection by default. Plain **Ctrl+C** exits the TUI.
- **tmux:** press the tmux prefix (default **Ctrl+B**), then **[**. Move to
  the text with the arrows. Start the selection with **Ctrl+Space** and copy
  with **Alt+W** in emacs mode, or use **Space** and **Enter** in vi mode.
  Prefix then **]** pastes from the tmux buffer. Copying to your workstation's
  clipboard instead requires support from your terminal and tmux configuration.
- **Linux virtual console (`/dev/ttyN`):** use tmux copy mode, or select and
  paste with the console mouse service (GPM) if configured. A bare console has
  no desktop clipboard.

After installation, `sudo btawg-server` opens the dashboard. Use ↑/↓ and Enter
or click a menu item. Start, stop, restart, autostart, logs, update, and removal
are available in the dashboard. Open **Service settings** to change launch
options. Saving a change restarts a running service; a stopped service stays
stopped. Removing either service keeps its installed launcher, `/etc/btawg`
settings and keys, and `/var/lib/btawg` server state.
For unattended installation, `sudo ./btawg-server install` still works.

Open **6881/TCP**, **6881/UDP**, **51821/UDP** (AWG), and **51820/UDP**
(WG) on the hub. DHT also needs outbound UDP. Real-TUN mode requires `ip`,
`sysctl`, and `nft` with kernel conntrack and NAT support. If the host
firewall drops traffic, allow the panel and **1053/TCP** (DNS) on the hub
TUN only, and allow forwarding from the TUN to the WAN plus established
return traffic. btawg configures routing, overlay addresses, and scoped NAT
automatically, but it **does not open host firewall ports** — add those
yourself.

### 2. Panel → first client

The panel listens on **localhost** and on the **Gateway** set for the
**Default** group in **Groups** (`http://10.77.0.1:8080` with the shipped
defaults), never on a wildcard or WAN interface. In real-TUN mode, saving a
different default-group gateway moves the VPN listener immediately; localhost
is unaffected. Before enrolling a VPN client, use an SSH port forward.

```sh
ssh -L 8080:127.0.0.1:8080 vps      # or open http://10.77.0.1:8080 over the VPN
```

Open `http://127.0.0.1:8080`, log in as `admin` with the initial password, then
choose **Clients → Add client → Create & get config**. Copy the `btawg://…`
string. It contains the client's private key; treat it as a secret.

### 3. Client machine

```sh
sudo ./btawg-client
```

Choose **Install service**, paste the `btawg://` import or [HTTPS subscription link](#https-profile-subscriptions)
into **VPN import** (hidden), then choose **Save and install**. The installer
saves the downloaded or pasted profile and generated SOCKS5 credentials under
`/etc/btawg`, then enables and starts `btawg-client.service`.
The imported mode starts enabled; other modes are reserved but off. Select a
transport and press Enter or Space to toggle it without restarting the service.
To edit the import, SOCKS5 address or base port, or TUN options, choose
**Service settings**. Saving changes restarts a running service. Leaving the
import field blank keeps it. In a text field, use your terminal's paste action (e.g.
right-click in PuTTY or the terminal menu); the TUI keeps the import hidden.
For unattended first-time installation, use
`sudo ./btawg-client install -import-file client.txt -socks-base-port 11080`.

The managed client reserves 13 consecutive local SOCKS5 ports on Linux and
Windows. **SOCKS5 base port** defaults to `1080` and accepts `1–65523` (the OS
may restrict low ports). For example, `11080` gives transports `11080–11089`
and balancers `11090–11092`. The tables below show the default numbers.
Toggling a mode off closes its sessions but keeps its port reserved. Changing
the base restarts a running client and closes its SOCKS5 sessions; point local
applications at the new ports. VPN, hub, and P2P settings are unchanged.

| Port | Mode | Port | Mode |
|---:|---|---:|---|
| 1080 | AWG · ext-utp | 1085 | WG · udp |
| 1081 | AWG · ext-tcp | 1086 | TLS · ext-utp |
| 1082 | AWG · req-utp | 1087 | TLS · ext-tcp |
| 1083 | AWG · req-tcp | 1088 | TLS · req-utp |
| 1084 | AWG · udp | 1089 | TLS · req-tcp |

Three additional SOCKS5 ports choose a tunnel for each new TCP connection or
UDP association. Existing sessions do not switch tunnels mid-stream.

| Port | Balancer | Chooses |
|---:|---|---|
| 1090 | Failover | First reachable tunnel in the configured priority order |
| 1091 | Round-robin | Next reachable tunnel in rotation |
| 1092 | Adaptive | Keeps the current reachable tunnel unless another is consistently better |

In the client dashboard, open a balancer's row, choose **Configure tunnels**,
check any of the ten tunnels, and select **Save tunnel selection**.
Each pool is saved separately and updated without restarting the service. Until customized, a pool follows the enabled
fixed modes. A custom pool can use a tunnel even if its fixed port is off.
Uncheck all tunnels to stop forwarding on that balancer; the port remains
reserved. Only new sessions can use a different tunnel. A balancer switches
transports, never the selected Internet route. It does not balance ordinary TUN traffic.

In **Failover** and **Adaptive**, highlight a tunnel and press **-** to move
it up or **+** (or the unshifted **=** key) to move it down. Failover takes
the first reachable checked tunnel. Adaptive uses the order to choose among
paths whose scores differ by less than its switching threshold. Adaptive
checks reachability and connection setup time and also learns upload and
download goodput separately from completed SOCKS5 TCP transfers. In the
absence of comparable recent transfers, it uses the latency threshold
(5 ms and 15%). A reachable alternative must stay better for two of its
probes before it becomes the preferred path. Adaptive occasionally routes
a new TCP connection through another reachable path to learn its speed;
it does not generate benchmark traffic or move existing connections. The
Adaptive dashboard row shows the currently preferred tunnel. An unavailable
path is replaced without waiting for two probes. A replacement BitTorrent
carrier needs a fresh successful probe; native AWG/WG still relies on the
background probes to detect loss of reachability.

Balancers test the tunnel against the hub's overlay control port (1054/TCP),
or the overlay DNS port (1053/TCP) on an older hub. No external website or
DNS lookup is used. If no selected path answers, new SOCKS5 requests fail.
The hub still learns a return path for unsolicited VPN traffic from authenticated
tunnel activity, so a newly connected path can receive new peer-initiated flows.
Replies to established SOCKS flows stay pinned to their original path.

All inbounds share **one SOCKS5 username/password** (RFC 1929). Choose **Show
SOCKS5 credentials** or **Rotate SOCKS5 credentials**; rotation is immediate
across every port and closes existing sessions. No-auth is refused. SOCKS5
does not encrypt these credentials. Expose these ports only to a trusted LAN.

For a single foreground connection (no service), the old form still works:

```sh
sudo ./btawg-client 'btawg://…'      # one mode, btawg0 interface
./btawg-client -no-tun 'btawg://…'   # no interface, SOCKS5-only mode
```

### Release downloads

| Platform | Client | Server |
| --- | --- | --- |
| Linux amd64 | `btawg-client-linux-amd64` | `btawg-server-linux-amd64` |
| Linux arm64 | `btawg-client-linux-arm64` | `btawg-server-linux-arm64` |
| Windows 11 amd64 | `btawg-client-windows-amd64.zip` | — |
| Windows 11 arm64 | `btawg-client-windows-arm64.zip` | — |

`SHA256SUMS` covers the binaries and the Windows desktop launcher ZIP. On
Linux, install the binary under the usual `btawg-client` or `btawg-server`
name regardless of architecture.

### Windows 11 client

Extract the ZIP for your architecture with `btawg-client.exe`, `wintun.dll`,
and `Wintun-LICENSE.txt` together. From an elevated Windows Terminal, run
`./btawg-client.exe` and use **Install service** with the same `btawg://` import
or HTTPS subscription link as on Linux. For unattended setup, run
`./btawg-client.exe install -import-file client.txt`. The native Windows
service starts at boot and works without the terminal. **Remove service**
leaves your configuration and installed bundle.

After installing the service, extract both files from
`btawg-desktop-launcher.zip` onto your desktop. Double-click
`start-btawg-client.cmd` and approve the UAC prompt to open the installed
dashboard. Keep `start-btawg-client.ps1` next to it. The launcher finds the
current installed executable after updates; it does not restart the service.
If Windows blocks the downloaded `.ps1`, review it, then use the **Unblock**
checkbox in its file properties.

In the **VPN import** field, press **Ctrl+V** or **F2** to paste from the
Windows clipboard. The import stays hidden; only its length is shown. In
**Show SOCKS5 credentials**, press **F2** to copy the username or **F3** to
copy the password, without selecting terminal text. Each shortcut copies only
that value into the Windows clipboard.

The Wintun interface uses the imported IPv4 address and routes, not a default
route. A conflicting LAN or VPN prefix is an error; fix the overlap before
starting the real-TUN client. The TUI, SOCKS5 ports, balance pools and peer
menus are shared with Linux. **View service logs** shows the Windows Application
event log entries for `btawg-client`.

Windows carrier sockets keep their selected physical interface between route
changes; btawg revalidates the selection when Windows reports network changes.
The userspace SOCKS packet pump waits when its transport queue is full. If
the upstream link queue also fills, it reports backpressure to the IP stack
instead of silently accepting another packet. The Adaptive details screen
shows cumulative queue saturation events since service start, once nonzero.

If service startup reports a SOCKS5 bind conflict on Windows, identify the
process listening on the reported TCP port (for example, `1080`):

```powershell
Get-NetTCPConnection -LocalPort 1080 -State Listen |
    Select-Object LocalAddress, LocalPort, OwningProcess,
        @{Name='Process';Expression={(Get-Process -Id $_.OwningProcess).ProcessName}}
```

Either free the port or choose a free block of 13 ports in **Service settings**
→ **SOCKS5 base port**, then choose **Start service** if the service is stopped.

The client does not change Windows Firewall. For direct connections allow
the configured P2P TCP and UDP listeners on the network where peers can reach
them, and the separate native AWG/WG peer UDP sockets when using those modes.
The internal P2P health check (TCP 1057) is answered inside the authenticated
direct tunnel and needs no Windows Firewall rule. On the TUN, allow the egress
listener (TCP 1055 when enabled) and any services you intend to expose by VPN
IP, scoped to the applicable VPN addresses. Leave unrelated inbound traffic blocked.

Both Windows release ZIPs run native component tests in GitHub Actions. For
manual verification, run `scripts/test-windows.ps1 -Bundle <zip>` in an
elevated test VM. For the additional x64 release gate, use
`scripts/windows-parity.ps1 -Bundle <zip> -Fixture
<json> -ExpectedSha256 <digest>` on Windows 11 x64 with a Linux hub and peer.
The fixture JSON supplies `ImportFile`, `VPNIP`, `EchoURL`, `EchoBody`,
`PeerEchoURL`, and `ObservationFile`. The observation JSON records `sha256`,
`platform` (`windows-11`), and a `scenarios` map with a `passed` boolean and
a link or path to evidence for each required case listed by the harness.
Do not include imports or credentials in the evidence.
Protect the `windows-client-parity` GitHub environment with required reviewers.
Set its `WINDOWS_11_VERIFIED_SHA256` secret after the passing run for the tagged
x64 ZIP. The release refuses a missing or different digest and publishes the
tested ZIPs unchanged.


### Update an installed service

In either dashboard (`sudo btawg-server` or `sudo btawg-client`), choose
**Update from GitHub**. The updater uses the host's regular Internet
connection, without SOCKS5 or proxy environment variables. It checks the
latest stable release in [GitHub Releases](https://github.com/h1w/btawg-tunnel/releases/latest)
and installs only a newer version after verifying its SHA-256. It selects the
asset for the installed operating system and architecture and keeps
settings, keys, and the service's autostart setting. A running service is
restarted; a stopped service stays stopped. On Windows the service selects a
complete EXE/DLL bundle at a time. If the new service fails to start, the
updater restores the previous executable or bundle and tries to restart it.

Both dashboards show the current update stage at the bottom of the screen.
During the download they show the percentage, or the amount downloaded if
GitHub does not provide a total size.

The dashboard stays responsive while the update runs. Press **Esc** to cancel
the download and return to the menu, or **Ctrl+C** to quit. If the executable
has already been replaced, the updater finishes the systemd restart or rollback
before quitting. Network failures and timeouts are shown in the dashboard.

The client dashboard checks release availability in the background when it
opens and shows a notice under the service status if a newer stable version
is available. The check does not download or install an update.

Linux amd64 installations from v0.11.1 or earlier still look for unsuffixed
release assets. Upgrade those installations manually once. Download the
matching executable and `SHA256SUMS` from the releases page and verify the
corresponding checksum. Then replace the installed binary and restart only
if its service is running. For the client:

```sh
sudo install -m 0755 btawg-client-linux-amd64 /usr/local/bin/btawg-client
sudo systemctl try-restart btawg-client.service
```

For the server, substitute `btawg-server` for `btawg-client` in both commands.
Subsequent updates work from the dashboard. Both binaries report their
version with `--version`.

### 4. Verify

```sh
curl --socks5-hostname 127.0.0.1:1080 --proxy-user "$SOCKS_LOGIN" https://example.com
curl http://10.77.0.1:8080/login
ip route get 10.77.0.1
```

Use an enabled port and set `SOCKS_LOGIN` to the username shown by **Show
SOCKS5 credentials**. Curl prompts for the password. SOCKS domain resolution runs at the hub.
The managed client owns one Linux TUN (`btawg0` by default) with the imported
VPN address and routes. Ordinary VPN-IP traffic uses the lowest-index enabled
transport. Fixed SOCKS5 ports use their assigned transport and balancer ports
choose one per new session, all through the same TUN.
Toggling modes does not recreate the interface or change the host default route.
The interface is removed when the service stops. A matching detached persistent
TUN from an old foreground client is adopted on upgrade.

### 5. Policy (panel, live)

In **Groups**, create an IPv4 subnet and set its peer visibility and
Internet policy. In **Clients**, choose a group when creating a user, or
move an existing user. Users inherit both group rules unless explicitly
overridden. The hub gives the user the same address in all ten modes.

Permission changes apply live, including closing disallowed active SOCKS
sessions. A move to another subnet or a changed group CIDR needs a new
import. Denied peer traffic is silently dropped.

**Revoke access** disables the client's key but keeps the record and VPN
address reserved. **Delete client** under **Clients → Access & actions**
permanently removes either an active or a revoked client and its traffic
history. The hub retires the old credentials before the address can be
allocated again.

### Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Client: `dht traversal finished: no peers` | Hub not announced yet — wait for its first announce, or the network blocks DHT → use `-dht-bootstrap` + `dht_bootstrap` in the import string |
| Client exits after ~45 s of retries | Hub unreachable (down / carrier port filtered) — check `6881/udp` and `-external-ip` |
| `curl` fails with IPv6 target | Hub host has no IPv6 — use `curl -4` (documented limit) |
| `tun setup (root required)` | Run with sudo or `-no-tun` |
| Service not installed | Open the binary with sudo and choose **Install service**. |
| Service failed to start | Choose **View service logs** to see the daemon error. |
| Initial panel password not saved | Check **Web panel and initial login**. Do not delete the database. |


## Run the server (VPS)

```sh
./btawg-server -store /var/lib/btawg/btawg.db
```

First boot generates and prints (once) the panel password and the deployment
infohash; the panel listens on `127.0.0.1:8080` **and** the hub's VPN address
(e.g. `10.77.0.1:8080`) — not the public IP. Reach it over the VPN, or via SSH
port-forward (`ssh -L 8080:127.0.0.1:8080 vps`) before a client exists. Flags:
`-transport` sets the preferred client mode, not which listeners are
enabled. `-carrier-port` and `-dht-bootstrap host:port[,host:port…]` apply
to the eight BitTorrent modes. `-external-ip` is the public IPv4 written into
imports; set it explicitly if the outbound interface address is not public.

All eight BitTorrent modes support a direct endpoint and DHT discovery. Native
AWG/WG use their explicit UDP endpoints. No mode silently falls back to
another. The panel exposes one `btawg://` import and native config
downloads for the same registered user.

**Server settings → Advanced AWG 3.1 settings** applies to AWG over
BitTorrent and native AWG, not WireGuard or TLS. New hubs provision a random
header protection key, enable random trailers, and retain `Jc=4`,
`Jmin=40`, `Jmax=100`, and `S1`–`S4=32`. Junk sizes are sampled independently;
two packets can have the same size. Existing deployments keep their profile
on upgrade. **Save AWG 3.1 preset** writes these settings as pending, retaining
the existing header key if present. `H1`–`H4` accept non-overlapping ranges or single values.
The header protection key is a deployment-wide secret shared by AWG peers;
the settings page never returns it. Leaving the key field blank keeps it;
use the clear checkbox to remove it. Imports and native exports contain the
key when configured and must be handled as secrets.

Upgrade the hub and AWG clients before enabling new wire settings. Save the
pending profile, restart the hub, and retrieve fresh imports or native AWG
configs from the active profile. Standalone native clients need an AWG engine
and tooling that support the selected options. Leave cookie protection
enabled unless you explicitly accept losing the handshake flood protections.

Carrier handshakes no longer include the product name: the hub uses a random
peer ID per process and the extended-message name is derived from the public
infohash. This removes literal identifiers, not protocol fingerprinting.

Ports: the carrier listens on **6881** (the canonical BitTorrent port,
override with `-carrier-port`), and every DHT socket in the system — hub
announcer and client discovery — binds the first free port of the standard
torrent client range **6881–6889**, falling back to an ephemeral port only if
the whole range is busy. A server + client on one machine coexist in the
range, exactly like two torrent clients would.

Updating both binaries enables the new hub AWG carriers automatically, with
the same SOCKS5 ports, IP routes and enabled modes:

- `ext-tcp` / `ext-utp`: mutually authenticated TLS-protected, packed and
  variably padded AWG records when the saved import already has a TLS hub pin.
- `req-tcp` / `req-utp`: variable blocks and an adaptive window starting at
  32 requests.

During a rolling upgrade, pairs that do not both advertise the new format
keep the previous framing. An old import without a TLS hub pin also keeps
the previous `ext` framing until refreshed. If protected `ext` is selected,
TLS authentication failure closes the connection without retrying in the
previous framing. The existing AWG profile still applies, including any
stored keys. Native modes, direct peer paths and TLS-only payload modes are
unchanged. Carrier pacing and idle keepalives remain off by default.

No environment variables are required. An existing `BTAWG_EXT_EXPERIMENT`
or `BTAWG_REQ_EXPERIMENT` override takes precedence. For comparison or
rollback, set the relevant variable to `baseline`; remove any overrides and
restart both services to restore automatic negotiation.

For a lossy WAN, set `BTAWG_UTP_PACE_MBPS` independently on each host to
cap that process's outgoing µTP bulk data (50–10000 Mbit/s; unset or `0`
disables it). Measure direct UDP goodput and packet loss in each direction
before tuning. The cap is not a promised application throughput; all µTP
peers on the hub share its pacing budget. TCP modes ignore this setting.

## Run a client (any PC)

```sh
./btawg-client -transport socks5-awg-ext-tcp 'btawg://…'
./btawg-client -transport socks5-req-tcp -dht 'btawg://…'
```

or `./btawg-client -config file.txt`. The client then:

- for a BitTorrent mode, connects to the imported endpoint or discovers it
  through DHT when requested; for the two native modes, connects directly over UDP
  to the selected native endpoint;
- serves **SOCKS5 on 127.0.0.1:1080**. Traffic
  goes directly into the tunnel as IPv4 packets; the hub routes and NATs
  them. The client resolves SOCKS domain targets via the hub's overlay-only
  DNS service, without asking the client's system DNS resolver. This single
  foreground form is an unauthenticated debug path; the managed service
  (`btawg-client install`) provides the authenticated per-mode inbounds;
- with real TUN, creates `btawg0` with the assigned VPN address and the
  managed IPv4 subnet routes from the import. Only sockets bound to that
  interface use its private egress routing table; the host default route
  stays unchanged. The hub still decides which traffic to forward.

The hub and client use a default tunnel MTU of 1280 bytes, including in
`-no-tun` mode and in exported standalone VPN configurations.

| AWG inside BitTorrent | TLS 1.3 inside BitTorrent (no AWG) | Outer transport |
|---|---|---|
| `socks5-awg-ext-tcp` | `socks5-ext-tcp` | Extended messages over TCP |
| `socks5-awg-req-tcp` | `socks5-req-tcp` | Request/piece over TCP |
| `socks5-awg-ext-utp` | `socks5-ext-utp` | Extended messages over µTP (UDP) |
| `socks5-awg-req-utp` | `socks5-req-utp` | Request/piece over µTP (UDP) |

`socks5-awg-udp` and `socks5-wg-udp` are native AWG and WG without BitTorrent.
SOCKS5 supports TCP and UDP in all modes; direct VPN IP traffic uses the same
tunnel without SOCKS5. The four TLS modes use the same IPv4 addresses and
hub ACLs as AWG/WG. Upgrade both binaries and re-export the import to get the
TLS server pin and discovery hashes. Existing imports and short names remain
usable in the original modes; there is no TLS-to-AWG fallback.

### Client-to-client

Dial another client's VPN IPv4 address for peer services. The hub must allow
communication in both directions. On managed Linux-TUN clients, enable
**Allow direct peer connections (P2P)** in **Service settings** and save, then
open **Peer connections → Global defaults** in the client dashboard:

- **Accept incoming** controls which of the ten transports a peer can use to
  initiate its own traffic to this device. An empty list accepts none.
- **Outgoing pools** configures independent **Failover**, **Round Robin**,
  and **Adaptive** pools. **Connect using** selects a single mode or a pool.
  Only modes accepted by the recipient can carry new direct flows. Replies
  use the same path. P2P pools do not change the 13 SOCKS5 exit selections.
- **Allow relay via central server** is off by default. With P2P enabled,
  peer traffic without a direct path is blocked unless relay is enabled on
  both ends. Hub gateway traffic and SOCKS5 Internet exits are separate.

To change these settings for one peer, open **Peer connections → Per-peer
overrides → Add by VPN IP**, enter its VPN IPv4 address (for example
`10.77.0.5`), and select the fields to override. Incoming transports, the
outgoing transport or policy, each of the three pools, and relay inherit
independently. **Use global** restores inheritance for that field;
**Remove override** restores it for the whole peer. An empty custom list is
not the same as inheriting: it allows no direct transport for that list.
Up to 32 peers can have overrides. Changes take effect without restarting
the client; direct paths are renegotiated through the hub. A directional
path still requires an outgoing mode that the other peer accepts, and
relay still requires permission at both endpoints. Update the client and
hub to use per-peer transport overrides.

Existing P2P configurations default to AWG `ext-utp` for incoming and outgoing
connections. Update both clients and the hub to negotiate transport selection.
The P2P listener uses distinct TCP and UDP candidates. UDP supports hole
punching and optional PCP, NAT-PMP and UPnP IGD mappings; TCP requires a
reachable listener and does not use UDP mappings. For a manual forward, set
a fixed **P2P listen port**, forward the appropriate protocol, and set its
matching **Public P2P UDP address** or **Public P2P TCP address**. Native AWG
and WG use separate UDP sockets. DHT discovers the
hub, not clients. Direct paths expire if the hub cannot renew their leases.
Peer endpoints are shared only with authorized peers.

For routers that reject finite IGD leases (`725 OnlyPermanentLeasesSupported`,
including MikroTik RouterOS), enable **Allow permanent UPnP mappings** in
**Service settings** alongside **Automatically map the peer UDP port**.
This is off by default. The client tries a finite lease first, checks that a
permanent mapping still belongs to it, and attempts to delete it on a clean
shutdown. After a crash or failed cleanup, remove any stale `btawg-*` mappings
for this device from the router. In MikroTik WinBox, check **IP → Firewall → NAT**.
Discovery and port mapping still require a reachable UPnP IGD and do not
open a port through an upstream NAT or the host firewall.

### Select a client exit or cascade

1. On each participating device, enable **Allow serving assigned egress routes**
   in **Service settings** and restart. This exposes only the VPN-side service.
2. In the server panel's **Routes** page, grant **Exit** or **Transit** to the
   intended devices. Create a named route with an exit and an optional first
   transit, then allow specific clients and/or groups. An empty audience grants
   no access. The hub's Internet setting controls only the central server exit.
3. In the client TUI, open a SOCKS5 port and choose **Egress route**.
   The selected ID follows the same transport or balancer if the port base
   changes. Each new TCP connection or UDP association uses that route; the
   final node resolves hostnames and opens public IPv4 destinations. Ordinary
   host Internet traffic is unchanged.

The same authorized hop may use a direct path or relay through the hub. If the
required hop or exit fails, no other exit or country is substituted. Active
route sessions are closed on permission withdrawal or lease expiry. The
transit is trusted and may see destinations; this is not an anonymity service.
Control and relay listen on 1054/TCP and 1056/TCP on the hub TUN only. An
egress node listens on 1055/TCP on the VPN only. If a host firewall filters
the TUN, permit these ports on that interface; do not expose them on WAN.
Update hub and clients for the new features; old imports continue working.

## Routing policy and groups

Each group owns a non-overlapping IPv4 CIDR and a hub address. New groups
allow communication within the group and deny internet by default. Group
visibility and user exceptions are mutual: both endpoints must allow the
other. Different transports do not create separate IP networks. These are
routed IPv4 networks, not shared Ethernet segments.

The hub enforces overlay policy and authorizes named egress routes separately.
A real-TUN hub routes its own IPv4 exit through scoped NAT and serves overlay
DNS. A `-no-tun` hub can coordinate and relay named routes but is not itself an
Internet exit. Kernel and userspace clients can use named exits, but direct P2P
requires a client kernel TUN. The client never installs a
system-wide default route. Standalone WG/AWG users must add the desired
public prefixes to `AllowedIPs` and the OS routes for Internet access.

Back up the database before upgrading. Existing IPs and settings move to
the default group; existing per-user permissions become explicit. Policy
edits are live. Changing the hub listener ports or AWG profile requires a
restart and a fresh import. In `-no-tun` mode the group gateway addresses
are fixed at startup; use real TUN to change group networks live.

Upgrade both binaries together for client SOCKS5 Internet access. Existing
imports still work with the updated client; the old hub SOCKS port is ignored.
An old client cannot proxy Internet traffic through an updated hub.

## Live activity and traffic

**Overview** shows recent activity, live upload and download speeds, and
cumulative VPN traffic. Search by client name, group or VPN IP. **Clients**
has the same live table plus access, group and revocation controls. The
visible page refreshes traffic every two seconds without refreshing forms.

**Active** means a verified handshake or an accepted upload in the last
three minutes. It does not prove that an idle WireGuard peer is still
online. The arrows show direction from the client's perspective. Counters
count inner IP bytes, not the BitTorrent/WG overhead on the public link;
client-to-client traffic counts at both ends. Traffic to the hub and
Internet traffic also count. Moving a user or switching transports does not
reset that user's totals.

The hub checkpoints totals to its existing database every 15 seconds and
on clean shutdown. Tracking starts on upgrade; pre-upgrade traffic cannot
be recovered. An unclean shutdown may lose traffic since the last checkpoint.

## Import string

`btawg://` + base64url(JSON) carries overlay addressing, connection chain,
endpoints and discovery hashes, the hub's public keys, and the client's
private key. Treat it as a secret and share it only over a trusted channel.
The BitTorrent handshake and DHT hash do not authenticate a peer: AWG or
the pinned mutual TLS keys authenticate each encrypted connection.

## HTTPS profile subscriptions

The hub can serve private `btawg://` profiles directly over HTTPS on **TCP
2228**. This listener is separate from the admin panel and VPN listeners.
Point a stable public DNS name at the hub and allow inbound TCP 2228. Supply
a PEM certificate chain trusted by the clients and a matching PEM private
key. The certificate must cover the subscription hostname.

For a managed server, set **Subscription public hostname**, **Subscription
certificate PEM file** and **Subscription private key PEM file** in the
server TUI's **Service settings**, or specify them on installation:

```sh
sudo ./btawg-server install \
  -subscription-host profiles.example.com \
  -subscription-cert /etc/letsencrypt/live/profiles.example.com/fullchain.pem \
  -subscription-key /etc/letsencrypt/live/profiles.example.com/privkey.pem
```

Use the same flags with `btawg-server run` in foreground mode. Use absolute
file paths for the service. After certificate renewal, restart the hub to
load the new files; existing links remain valid. Leave all three arguments
unset (or clear all three saved TUI fields) to disable the listener.

1. In the web panel, go to **Clients → Import config → Issue subscription
   link**, then copy the `https://…:2228/sub/…` URL. Treat it like the private
   import string: anyone with the link can download the profile. **Rotate
   subscription link** invalidates the previous URL without changing VPN keys.
   Revoking or deleting a client also disables its downloads.
2. Paste the URL into **VPN import** on Linux or Windows, or put it in the
   secret file passed to `install -import-file`. To switch an existing client,
   paste the link into **Service settings**. A static `btawg://` import removes
   the subscription association.
3. Use **Refresh subscription** in the client menu after changes to the active
   hub configuration. An unchanged profile or failed fetch leaves the current
   profile and running service alone. A changed profile preserves the enabled
   transports and local settings; a running service restarts and a stopped
   service stays stopped. There is no automatic refresh.

## Verification performed

- Carrier framing, SOCKS5 (CONNECT + UDP ASSOCIATE), bbolt store, DHT
  announce/discover (loopback relay), panel handlers: unit tests.
- Full suite `go test ./...` green incl. `-race` on the networking paths.
- End-to-end in all eight BitTorrent modes: DHT discovery through a local
  relay, local SOCKS5 destination filtering and client-to-client IPv4 traffic.
  The mixed-mode test also routes between both native modes and the carriers.
- Managed services: a Linux VM with real systemd, kernel TUN, nftables NAT,
  and separate hub, client, and egress networks exercised both installers,
  login on loopback and access to the panel over the VPN, ten authenticated
  SOCKS5 paths (hostname TCP and UDP), live TUI switching and password
  rotation, and persistence after restart and reinstall.

On a single Linux host, separate network namespaces connected by a kernel
`netkit` link (RTT approximately 0.06 ms) transferred and verified 1 GiB per
mode and direction through real TUN interfaces, AWG, and SOCKS5. The
benchmark supplies the discovered peer address locally; it measures bulk
transfer after connect, not DHT or session setup. The speeds below are
application goodput, not link rate.

| Carrier | 1 stream down | 1 stream up | 4 streams down | 4 streams up |
|---|---:|---:|---:|---:|
| `ext-tcp` | 1.846 | 2.288 | 2.306 | 2.372 |
| `ext-utp` | 1.484 | 1.514 | 1.445 | 1.443 |
| `req-tcp` | 2.236 | 2.256 | 2.222 | 2.204 |
| `req-utp` | 1.349 | 1.375 | 1.308 | 1.303 |

All values are in Gbit/s. Linux batches IPv4 µTP sends and receives. Windows
batches eligible outgoing µTP datagrams with UDP segmentation when supported
and otherwise sends ordinary datagrams. Segmentation does not change wire
datagram boundaries. The figures above are Linux results; they do not
describe a physical 1 GbE link, public WAN path, or Windows throughput.

### Two-VPS WAN measurement

One external carrier per client. Each run transferred and verified 512 MiB
through real TUN, AWG and SOCKS5: four application flows for TCP, two for
µTP. The table reports medians and ranges of three runs per direction at
fixed settings, in Mbit/s. Download is hub to client.

| Carrier | Download median (range) | Upload median (range) | Median vs. direct, down / up |
|---|---:|---:|---:|
| `ext-tcp` | 821.3 (694.8–852.7) | 795.4 (778.0–873.9) | 93.4% / 80.2% |
| `req-tcp` | 738.4 (718.4–760.8) | 823.8 (789.5–832.1) | 84.0% / 83.0% |
| `ext-utp` | 181.3 (177.8–181.9) | 279.6 (253.7–281.4) | 76.2% / 74.0% |
| `req-utp` | 175.6 (172.7–182.9) | 294.5 (253.1–295.2) | 73.8% / 78.0% |

TCP percentages use the original four-stream direct control of 879/992
Mbit/s. A fresh direct TCP run on the same path measured 944.2/639.1.
Direct UDP delivered 237.9/377.7 Mbit/s with 1382-byte datagrams over six
seconds, offered at 250/390 Mbit/s (4.6%/2.9% packet loss).

Set `BTAWG_UTP_PACE_MBPS` to 275 on the hub and 410 on the client for
the measured `ext-utp` profile, or 355 and 455 respectively for `req-utp`.
These are pacing ceilings for this route, not throughput guarantees.
Both µTP upload medians exceeded 70% of direct UDP; each mode also had
one run below that threshold.

## Honest limits

- BitTorrent framing substitutes encrypted traffic for file blocks. Piece
  hashes never validate; no metadata is served. Existing nDPI 6.1.0 results
  cover the historical AWG carrier, not the new TLS modes. Active probing is out
  of scope. No claim of universal undetectability.
- Native AWG adds the profile's `S4` bytes on top of the normal tunnel
  overhead. If large transfers fail with `message too long`, check the
  outer path MTU. This overhead does not apply to the BitTorrent modes.
- DHT discovery depends on bootstrap reachability; if a network blocks the
  public DHT, point both sides at a private bootstrap island with
  `-dht-bootstrap` (hub) and the import string's `dht_bootstrap` field.
- DHT returns the address observed by a DHT node; a NAT'd hub needs a
  reachable public forwarding port. A client does not need an inbound port
  to use the hub. Direct peer paths are opportunistic; a restrictive NAT or
  firewall can leave the hub as the only viable path.
- A local latency benchmark is not a WAN throughput guarantee. Measure raw
  TCP to the same hub in both directions and compare it with completed SOCKS
  transfers of the same size. A speed test to a different host is not a
  baseline for the VPN path. `ext-*` rounds the datagram plus its length field
  up to 32 bytes rather than padding every frame to 2 KiB. `req-*` uses
  1536-byte blocks between new peers and 2048-byte blocks with old peers.
  For a long-distance benchmark, apply 100 ms one-way delay in each
  direction (200 ms RTT total) to the carrier only. Place `netem` on an
  intermediate router's egress toward each endpoint, not on the TCP
  sender's egress, where TCP Small Queues can distort the result.
  Keep the SOCKS endpoint and test source local to their respective hosts,
  verify the complete payload at the receiver, and report upload and
  download separately.
  In an isolated Linux run at 201 ms RTT, all four modes delivered 512 MiB
  through real TUN and SOCKS5 in four parallel TCP streams per direction.
  Download/upload goodput was 559/564 Mbit/s (`ext-tcp`), 535/533 (`req-tcp`),
  547/542 (`ext-utp`), and 516/514 (`req-utp`). Direct TCP reached 719/719
  Mbit/s on the same lab path. The run used an already-established AWG
  session and injected the discovery result; it does not measure startup,
  single-stream speed, or a public WAN. Earlier `req-utp` tuning trials
  also fell below 500 Mbit/s, so the result is not a sustained minimum.
  Real-TUN mode uses Reno and a bounded send buffer on the inner SOCKS TCP
  sockets and a larger transmit queue on the overlay TUN. The outer TCP
  carrier uses BBR where available; other TCP sockets keep the host default.
  Loopback SOCKS CONNECT legs use bounded buffers. No host-wide TCP setting
  or WAN qdisc is changed.
- No IPv6 overlay, rate limits, LAN-behind-client routing, or kill switch.
  Revocation removes the peer from both engines and closes its carriers.
- Hub real-TUN mode requires `CAP_NET_ADMIN`. Run it on the VPS with
  `sudo ./btawg-server` or use the systemd unit above.
- Disallowed client↔client dials are silently dropped by design, so they time
  out rather than returning a clean error or RST.
