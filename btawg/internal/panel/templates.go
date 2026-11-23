package panel

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed panel.css
var panelCSS string

//go:embed panel.js
var panelJS string

// layout is shared by all server-rendered screens.
const layout = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>btawg</title>
<link rel="stylesheet" href="/assets/panel.css">
<script src="/assets/panel.js" defer></script>
</head>
<body>
<header><div class="top"><a class="brand" href="/"><span class="mark" aria-hidden="true">◈</span> btawg</a>
{{if .Active}}
<nav aria-label="Main navigation">
<a href="/" {{if eq .Active "dashboard"}}class="active" aria-current="page"{{end}}>Overview</a>
<a href="/clients" {{if eq .Active "clients"}}class="active" aria-current="page"{{end}}>Clients</a>
<a href="/groups" {{if eq .Active "groups"}}class="active" aria-current="page"{{end}}>Groups</a>
<a href="/routes" {{if eq .Active "routes"}}class="active" aria-current="page"{{end}}>Routes</a>
<a href="/server" {{if eq .Active "server"}}class="active" aria-current="page"{{end}}>Server</a>
</nav>
<form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="quiet">Sign out</button></form>
{{end}}
</div></header>
<main>{{template "content" .}}</main>
</body></html>`

const loginTmpl = `{{define "content"}}
<div class="card form-card">
<h1>Sign in</h1>
<p class="intro">Manage your VPN hub.</p>
{{if .Error}}<div class="err" role="alert">{{.Error}}</div>{{end}}
<form class="stack" method="post" action="/login">
<label>Username<input name="username" autocomplete="username" required></label>
<label>Password<input type="password" name="password" autocomplete="current-password" required></label>
<button>Sign in</button>
</form>
</div>
{{end}}`

const dashboardTmpl = `{{define "content"}}
<div data-live>
<div class="page-head"><div><span class="eyebrow">VPN hub</span><h1>Overview</h1><p class="intro">Live client activity and total VPN traffic.</p></div><a class="button" href="/clients#new-client">Add client</a></div>
<section class="summary" aria-label="Traffic summary">
<div class="card"><span>Recently active / registered</span><strong data-metric="active">— / {{len .Clients}}</strong></div>
<div class="card"><span>↑ Upload from clients</span><strong data-metric="up">—</strong></div>
<div class="card"><span>↓ Download to clients</span><strong data-metric="down">—</strong></div>
<div class="card"><span>Traffic since tracking began</span><strong data-metric="total">—</strong></div>
</section>
{{template "live-table" .}}
</div>
{{end}}`

const liveTableTmpl = `{{define "live-table"}}
<section class="card" data-live-table>
<div class="toolbar"><div><h2>Clients</h2><span class="sub" data-health role="status">Waiting for live data…</span><a class="sub" data-reload href="" hidden>Client list changed. Reload to update.</a></div>
{{if .Clients}}<label><input type="search" data-filter placeholder="Filter by name, group or IP" aria-label="Filter by name, group or IP"></label>{{end}}</div>
{{if .Clients}}
<div class="scroll live-scroll" tabindex="0" role="region" aria-label="Client activity table">
<table class="live-table" role="table" aria-label="Client activity"><thead role="rowgroup"><tr role="row"><th role="columnheader" scope="col">Client / Group</th><th role="columnheader" scope="col">VPN address</th><th role="columnheader" scope="col">Activity / Last seen</th><th role="columnheader" scope="col">Current speed</th><th role="columnheader" scope="col">Total VPN traffic</th><th role="columnheader" scope="col">Actions</th></tr></thead><tbody role="rowgroup">
{{range .Clients}}{{$group := index $.GroupNames .GroupID}}{{$live := index $.Live .ID}}
<tr id="client-{{.ID}}" role="row" data-client="{{.ID}}" data-revoked="{{.Revoked}}" data-search="{{.Name}} {{$group}} {{.IP}}">
<td role="cell" class="identity"><strong>{{.Name}}</strong><span class="sub">{{$group}}</span></td><td role="cell" class="address numbers">{{.IP}}</td>
<td role="cell" class="live-state"><span data-status class="tag{{if eq $live.Status "active"}} ok{{else if .Revoked}} revoked{{end}}">{{statusLabel $live.Status}}</span><span class="sub" data-seen>{{seenAt $live.LastSeen}}</span></td>
<td role="cell" class="rates numbers"><span class="mobile-label">Current speed</span><span data-rates>↑ — · ↓ —</span></td>
<td role="cell" class="usage numbers"><span class="mobile-label">Total VPN traffic</span><strong data-total>{{byteSize (sum $live.Up $live.Down)}}</strong><span class="sub" data-breakdown>↑ {{byteSize $live.Up}} · ↓ {{byteSize $live.Down}}</span></td>
<td role="cell" class="actions">{{if .Revoked}}<span class="sub">Access disabled</span>{{else}}<a href="/clients/import?id={{.ID}}">Import config</a>{{end}}
{{if eq $.Active "clients"}}{{template "client-actions" (clientActions . $)}}{{else}}<span class="sub"><a href="/clients#client-{{.ID}}">Manage</a></span>{{end}}</td></tr>
{{end}}
</tbody></table></div>
<p class="empty" data-no-matches hidden>No clients match your search.</p>
{{else}}<p class="empty">No clients yet. <a href="/clients#new-client">Add your first client</a>.</p>{{end}}
<p class="hint">Active means a verified handshake or upload in the past 3 minutes. Counters include traffic to the hub, other clients and the Internet. ↑ and ↓ are from each client’s perspective.</p>
<noscript><p class="note">Enable JavaScript for live speeds and automatic refresh.</p></noscript>
</section>
{{end}}`

const serverTmpl = `{{define "content"}}
<h1>Server settings</h1>
{{if .Saved}}<div class="tag ok">Saved. Restart the hub to apply pending settings; re-import affected AWG clients after restart.</div>{{end}}
<div class="note">The form shows pending settings. Restart the hub to apply changes. AWG imports and exports use the active profile.</div>
<form method="post" action="/server">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<div class="card"><h2>AWG profile (BitTorrent + native AWG)</h2>
<div class="grid">
<label>jc<input name="jc" value="{{.Cfg.AWGProfile.JC}}"></label>
<label>jmin<input name="jmin" value="{{.Cfg.AWGProfile.JMin}}"></label>
<label>jmax<input name="jmax" value="{{.Cfg.AWGProfile.JMax}}"></label>
<label>s1<input name="s1" value="{{.Cfg.AWGProfile.S1}}"></label>
<label>s2<input name="s2" value="{{.Cfg.AWGProfile.S2}}"></label>
<label>s3<input name="s3" value="{{.Cfg.AWGProfile.S3}}"></label>
<label>s4<input name="s4" value="{{.Cfg.AWGProfile.S4}}"></label>
<label>h1<input name="h1" value="{{.Cfg.AWGProfile.H1}}"></label>
<label>h2<input name="h2" value="{{.Cfg.AWGProfile.H2}}"></label>
<label>h3<input name="h3" value="{{.Cfg.AWGProfile.H3}}"></label>
<label>h4<input name="h4" value="{{.Cfg.AWGProfile.H4}}"></label>
</div>
<details>
<summary>Advanced AWG 3.1 settings</summary>
<p class="hint">H1–H4 and the range fields accept a number or a range such as 100–200 (type an ASCII hyphen). Blank optional fields keep engine defaults.</p>
<div class="grid">
<label>I1<input name="i1" value="{{.Cfg.AWGProfile.I1}}"></label>
<label>I2<input name="i2" value="{{.Cfg.AWGProfile.I2}}"></label>
<label>I3<input name="i3" value="{{.Cfg.AWGProfile.I3}}"></label>
<label>I4<input name="i4" value="{{.Cfg.AWGProfile.I4}}"></label>
<label>I5<input name="i5" value="{{.Cfg.AWGProfile.I5}}"></label>
<label>Header protection key (64 hex digits)<input type="password" name="header_protection_key" autocomplete="new-password" placeholder="{{if .AWGKeyConfigured}}Configured; blank keeps current key{{else}}Not configured{{end}}"></label>
<label>Content padding addition (bytes)<input name="content_padding_addition" value="{{.Cfg.AWGProfile.ContentPaddingAddition}}"></label>
<label>Rekey after (seconds)<input name="rekey_after_time" value="{{.Cfg.AWGProfile.RekeyAfterTime}}"></label>
<label>Rekey timeout (seconds)<input name="rekey_timeout" value="{{.Cfg.AWGProfile.RekeyTimeout}}"></label>
<label>Reject keys after (seconds)<input name="reject_after_time" value="{{.Cfg.AWGProfile.RejectAfterTime}}"></label>
<label>Keepalive timeout (seconds)<input name="keepalive_timeout" value="{{.Cfg.AWGProfile.KeepaliveTimeout}}"></label>
<label>Max handshake attempts<input name="max_handshake_attempts" value="{{.Cfg.AWGProfile.MaxHandshakeAttempts}}"></label>
<label>Client persistent keepalive (seconds)<input name="persistent_keepalive" value="{{.Cfg.AWGProfile.PersistentKeepalive}}" placeholder="Mode default; 0 disables"></label>
</div>
<div class="row">
<label><input type="checkbox" name="clear_header_protection_key" value="1"> Clear header protection key</label>
<label><input type="checkbox" name="random_trailers" value="1"{{if .Cfg.AWGProfile.RandomTrailers}} checked{{end}}> Random trailers</label>
<label><input type="checkbox" name="disable_cookies" value="1"{{if .Cfg.AWGProfile.DisableCookies}} checked{{end}}> Disable cookies and handshake flood protection</label>
</div>
<p class="hint">The AWG 3.1 preset resets these AWG fields to deployment defaults and generates a key if none exists. Existing keys are kept. The hub and clients continue using the active profile until restart and re-import.</p>
<button type="submit" formaction="/server/awg31-preset">Save AWG 3.1 preset</button>
</details>
{{if .AWGProfileChanged}}<div class="note">Profile differs from active config — restart the hub, then re-import AWG clients.</div>{{end}}
</div>
<div class="card"><h2>Preferred mode &amp; transport</h2>
<div class="row">
<div><label>Mode
<select name="transport">
{{$t := .Cfg.Transport}}
{{range .Modes}}<option value="{{.}}" {{if eq $t .}}selected{{end}}>{{.}}</option>{{end}}
</select></label></div>
<div><label>Carrier port{{if .Managed}}<input readonly value="{{.Cfg.CarrierPort}}">{{else}}<input name="carrier_port" value="{{.Cfg.CarrierPort}}">{{end}}</label></div>
<div><label>External IP (announce &amp; native)<input name="external_ip" value="{{.Cfg.ExternalIP}}" placeholder="autodetect"></label></div>
</div>
<div class="note">Carrier modes use the BitTorrent framing. Native modes require ExternalIP to be set.</div>
{{if ne .Cfg.Transport .ActiveCfg.Transport}}<div class="note">Transport differs from active config — restart required.</div>{{end}}
</div>
<div class="card"><h2>Ports</h2>
{{if .Managed}}<div class="note">Managed service: carrier, AWG and WG ports are fixed. Only the panel port is editable.</div>{{end}}
<div class="row">
<div><label>Native AWG UDP{{if .Managed}}<input readonly value="{{.Cfg.AWGPort}}">{{else}}<input name="awg_port" value="{{.Cfg.AWGPort}}">{{end}}</label></div>
<div><label>Native WG UDP{{if .Managed}}<input readonly value="{{.Cfg.WGPort}}">{{else}}<input name="wg_port" value="{{.Cfg.WGPort}}">{{end}}</label></div>
<div><label>Panel (loopback)<input name="panel_port" value="{{.Cfg.PanelPort}}"></label></div>
</div>
{{if or (ne .Cfg.CarrierPort .ActiveCfg.CarrierPort) (ne .Cfg.AWGPort .ActiveCfg.AWGPort) (ne .Cfg.WGPort .ActiveCfg.WGPort) (ne .Cfg.PanelPort .ActiveCfg.PanelPort)}}<div class="note">Port changes differ from active config — restart required.</div>{{end}}
</div>
<button>Save settings</button>
</form>
{{end}}`

const clientsTmpl = `{{define "content"}}
<div data-live>
<div class="page-head"><div><span class="eyebrow">Access</span><h1>Clients</h1><p class="intro">Add devices, manage access and retrieve client configuration.</p></div></div>
<section class="card" id="new-client"><h2>Add client</h2>
<form method="post" action="/clients/create">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<div class="row"><div><label>Device name (optional)<input name="name" placeholder="Laptop"></label></div>
<div><label>Group<select name="group_id">{{range .Groups}}<option value="{{.ID}}" {{if eq .ID "default"}}selected{{end}}>{{.Name}}</option>{{end}}</select></label></div>
<button>Create &amp; get config</button></div>
</form>
</section>
{{template "live-table" .}}
</div>
{{end}}`

const clientActionsTmpl = `{{define "client-actions"}}
{{$c := .Client}}
<details><summary>Access &amp; actions</summary>
{{if not $c.Revoked}}
<form class="stack" method="post" action="/clients/policy">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="id" value="{{$c.ID}}">
<label>Internet<select name="internet">
<option value="inherit" {{if not $c.Internet}}selected{{end}}>Use group setting</option>
<option value="on" {{if and $c.Internet (deref $c.Internet)}}selected{{end}}>Allow</option>
<option value="off" {{if and $c.Internet (not (deref $c.Internet))}}selected{{end}}>Block</option>
</select></label>
<label>Visible clients<select name="peers" multiple size="4">
<option value="inherit" {{if unset $c.VisiblePeers}}selected{{end}}>Use group setting</option>
<option value="none" {{if and (not (unset $c.VisiblePeers)) (not $c.VisiblePeers)}}selected{{end}}>Nobody</option>
<option value="*" {{if contains $c.VisiblePeers "*"}}selected{{end}}>Everyone</option>
{{range .Clients}}{{if and (ne .ID $c.ID) (not .Revoked)}}<option value="{{.ID}}" {{if contains $c.VisiblePeers .ID}}selected{{end}}>{{.Name}}</option>{{end}}{{end}}
</select></label>
<button class="quiet">Save access</button>
</form>
<form class="stack" method="post" action="/clients/move">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="id" value="{{$c.ID}}">
<label>Move to group<select name="group_id">{{range .Groups}}<option value="{{.ID}}" {{if eq .ID $c.GroupID}}selected{{end}}>{{.Name}}</option>{{end}}</select></label>
<span class="sub">Changing groups assigns a new VPN address. Import the config again.</span>
<button class="quiet">Move client</button>
</form>
<form method="post" action="/clients/revoke" onsubmit="return confirm('Revoke access for this client?')">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="id" value="{{$c.ID}}">
<button class="warn">Revoke access</button></form>
{{end}}
<form method="post" action="/clients/delete" onsubmit="return confirm('Permanently delete this client and its traffic history? The VPN address will be available for reuse.')">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="id" value="{{$c.ID}}">
<button class="warn">Delete client</button></form>
</details>
{{end}}`

const importTmpl = `{{define "content"}}
<h1>Import string — {{.Client.Name}}</h1>
<div class="card"><h2>btawg:// client bootstrap</h2>
<p>This string contains the client's private key. Copy it over a trusted channel and paste it into <b>Install service</b> in the client TUI.</p>
<pre class="mono" id="import-string">{{.Import}}</pre>
<p><button type="button" class="quiet" data-copy="import-string">Copy import</button> <span class="sub" data-copy-status role="status"></span></p>
<p class="note">New imports connect directly. For foreground operation, pass the import to <code>btawg-client run</code>.</p>
</div>
<div class="card"><h2>Native config export</h2>
<p>For standard WireGuard/AmneziaWG apps (requires ExternalIP):</p>
<p>
<a href="/clients/native?id={{.Client.ID}}&mode=socks5-awg-udp"><button>Standalone AmneziaWG config</button></a>
<a href="/clients/native?id={{.Client.ID}}&mode=socks5-wg-udp"><button>Standalone WireGuard config</button></a>
</p>
</div>
<p><a href="/clients">← back to clients</a></p>
{{end}}`

const nativeTmpl = `{{define "content"}}
<h1>Native {{.Mode}} config — {{.Client.Name}}</h1>
<div class="card"><h2>WireGuard/AmneziaWG import text</h2>
<p>Paste into the native app. The text contains keys and endpoint only; routes are managed.</p>
<pre class="mono">{{.Config}}</pre>
<p class="note">Do not share the private_key value.</p>
<p><a href="/clients/import?id={{.Client.ID}}">← back to import</a></p>
</div>
{{end}}`

const groupsTmpl = `{{define "content"}}
<h1>Groups</h1>
<p class="intro">Organize clients into VPN subnets and set their default access.</p>
<p><a class="button" href="/groups/edit">Add group</a></p>
<div class="card"><h2>Groups</h2>
{{if .Groups}}
<table><tr><th>Name</th><th>Subnet</th><th>Gateway</th><th>Internet</th><th>Visible groups</th><th></th></tr>
{{range .Groups}}
<tr>
<td><a href="/groups/edit?id={{.ID}}">{{.Name}}</a></td>
<td>{{.Subnet}}</td>
<td>{{.ServerIP}}</td>
<td>{{if .NoInternet}}off{{else}}on{{end}}</td>
<td>{{if unset .VisibleGroups}}same group{{else if not .VisibleGroups}}none{{else if contains .VisibleGroups "*"}}all{{else}}{{range .VisibleGroups}}{{.}} {{end}}{{end}}</td>
<td>{{if ne .ID "default"}}<form method="post" action="/groups/delete" onsubmit="return confirm('Delete this empty group?')">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}">
<button class="warn">Delete</button></form>{{end}}</td>
</tr>
{{end}}
</table>
{{else}}<div class="tag off">no custom groups; all clients use the default group</div>{{end}}
</div>
{{end}}`

const groupEditTmpl = `{{define "content"}}
<h1>{{if .Group.ID}}Edit{{else}}Create{{end}} group</h1>
<form method="post" action="/groups/save">
<input type="hidden" name="csrf" value="{{.CSRF}}">
{{if .Group.ID}}<input type="hidden" name="id" value="{{.Group.ID}}">{{end}}
<div class="card">
<div class="row">
<div><label>Name<input name="name" value="{{.Group.Name}}" placeholder="work-net"></label></div>
<div><label>Subnet<input name="subnet" value="{{.Group.Subnet}}" placeholder="10.77.1.0/24"></label></div>
<div><label>Gateway<input name="server_ip" value="{{.Group.ServerIP}}" placeholder="10.77.1.1"></label></div>
</div>
<div class="row" style="margin-top:10px">
<div><label style="white-space:nowrap"><input type="checkbox" name="internet" value="on" {{if and .Group.ID (not .Group.NoInternet)}}checked{{end}}> internet access</label></div>
</div>
<div class="row">
<div><label>Visible groups
<select name="visible_mode">
<option value="same" {{if unset .Group.VisibleGroups}}selected{{end}}>same group only</option>
<option value="none" {{if and (not (unset .Group.VisibleGroups)) (not .Group.VisibleGroups)}}selected{{end}}>none</option>
<option value="all" {{if contains .Group.VisibleGroups "*"}}selected{{end}}>all groups</option>
<option value="list" {{if and .Group.VisibleGroups (not (contains .Group.VisibleGroups "*"))}}selected{{end}}>listed groups</option>
</select></label></div>
<div><label>Listed group IDs (comma-separated)<input name="visible_groups" value="{{if and .Group.VisibleGroups (not (contains .Group.VisibleGroups "*"))}}{{range .Group.VisibleGroups}}{{.}},{{end}}{{end}}" placeholder="group-a, group-b"></label></div>
</div>
</div>
<button>Save group</button>
</form>
{{end}}`

var layoutTmpl = template.Must(template.New("layout").Parse(layout))

var pages = map[string]*template.Template{}

func init() {
	funcs := template.FuncMap{
		"contains": func(haystack []string, needle string) bool {
			for _, s := range haystack {
				if s == needle {
					return true
				}
			}
			return false
		},
		"deref": func(b *bool) bool {
			return b != nil && *b
		},
		"unset":         func(v []string) bool { return v == nil },
		"clientActions": actionsData,
		"statusLabel":   statusLabel,
		"seenAt":        seenAt,
		"byteSize":      byteSize,
		"sum":           func(a, b uint64) uint64 { return a + b },
	}
	for name, body := range map[string]string{
		"login":      loginTmpl,
		"dashboard":  dashboardTmpl + liveTableTmpl + clientActionsTmpl,
		"server":     serverTmpl,
		"clients":    clientsTmpl + liveTableTmpl + clientActionsTmpl,
		"import":     importTmpl,
		"native":     nativeTmpl,
		"groups":     groupsTmpl,
		"group-edit": groupEditTmpl,
		"routes":     routesTmpl,
		"route-edit": routeEditTmpl,
	} {
		clone, err := layoutTmpl.Clone()
		if err != nil {
			panic(err)
		}
		clone.Funcs(funcs)
		pages[name] = template.Must(clone.Parse(body))
	}
}

func (p *Panel) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[name].ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
