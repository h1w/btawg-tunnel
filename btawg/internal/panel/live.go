package panel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"bittorrent-tunnel/btawg/internal/awgrepair"
	"bittorrent-tunnel/btawg/internal/store"
)

const recentActivity = 3 * time.Minute

type liveClient struct {
	store.Traffic
	Status string `json:"status"`
}

func clientLive(revoked bool, traffic store.Traffic, now time.Time) liveClient {
	status := "never"
	switch {
	case revoked:
		status = "revoked"
	case traffic.LastSeen == 0:
	case now.Sub(time.UnixMilli(traffic.LastSeen)) <= recentActivity:
		status = "active"
	default:
		status = "idle"
	}
	return liveClient{Traffic: traffic, Status: status}
}

func (p *Panel) liveStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	clients, err := p.st.ClientRevocations()
	if err != nil {
		http.Error(w, "could not load clients", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	traffic := p.reg.Traffic()
	data := struct {
		At               int64                 `json:"at"`
		Clients          map[string]liveClient `json:"clients"`
		EngineQueueDrops map[string]uint64     `json:"engine_queue_drops"`
		AWGRecovery      awgrepair.Stats       `json:"awg_recovery"`
	}{At: now.UnixMilli(), Clients: make(map[string]liveClient, len(clients)), EngineQueueDrops: p.reg.EngineQueueDrops(), AWGRecovery: p.reg.AWGRepairStats()}
	for id, revoked := range clients {
		data.Clients[id] = clientLive(revoked, traffic[id], now)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func groupNames(groups []store.GroupRecord) map[string]string {
	names := make(map[string]string, len(groups))
	for _, g := range groups {
		names[g.ID] = g.Name
	}
	return names
}

func panelAsset(contentType, body string) http.HandlerFunc {
	tag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", tag)
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
	}
}

func statusLabel(status string) string {
	switch status {
	case "active":
		return "Active"
	case "idle":
		return "Idle"
	case "revoked":
		return "Revoked"
	default:
		return "Never seen"
	}
}

func seenAt(ms int64) string {
	if ms == 0 {
		return "—"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC")
}

func byteSize(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"} {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	panic("unreachable")
}

type clientActionData struct {
	Client  store.ClientRecord
	CSRF    string
	Clients []store.ClientRecord
	Groups  []store.GroupRecord
}

func actionsData(rec store.ClientRecord, page map[string]any) clientActionData {
	return clientActionData{rec, page["CSRF"].(string), page["Clients"].([]store.ClientRecord), page["Groups"].([]store.GroupRecord)}
}
