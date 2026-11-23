// Package store persists the btawg hub state in an embedded bbolt database:
// the server configuration (bucket `server`, single key), one record per
// group (bucket `groups`, key = group ID) and one record per client
// (bucket `clients`, key = client ID). All values are JSON.
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	bolt "go.etcd.io/bbolt"

	"bittorrent-tunnel/btawg/internal/config"
)

var (
	bucketServer  = []byte("server")
	bucketGroups  = []byte("groups")
	bucketClients = []byte("clients")
	serverKey     = []byte("config")
)

// DefaultGroupID is the legacy group created from ServerConfig on first open.
const DefaultGroupID = "default"

// ErrNotFound is returned by GetClient for an unknown ID.
var ErrNotFound = errors.New("client not found")

// ErrDuplicateIP and ErrDuplicatePeerID are returned by CreateClient when the
// new record collides with an existing client.
var (
	ErrDuplicateIP     = errors.New("client IP already in use")
	ErrDuplicatePeerID = errors.New("client peer ID already in use")
)

// Group and subnet errors.
var (
	ErrGroupNotFound   = errors.New("group not found")
	ErrInvalidSubnet   = errors.New("invalid subnet")
	ErrSubnetOverlap   = errors.New("subnet overlaps existing group")
	ErrSubnetExhausted = errors.New("subnet exhausted")
	ErrGroupNotEmpty   = errors.New("group has clients")
	ErrDefaultGroup    = errors.New("cannot delete the default group")
)

// GroupRecord is one deployed group: its IPv4 subnet, the hub address inside
// it, and the default reachability policy applied to group-managed users.
//
// VisibleGroups controls which other groups are reachable by default:
//   - nil means "same group only";
//   - an empty slice means "no groups";
//   - []string{"*"} means "all groups";
//   - a list of group IDs means exactly those groups.
type GroupRecord struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Subnet        string   `json:"subnet"`
	ServerIP      string   `json:"server_ip"`
	NoInternet    bool     `json:"no_internet"`
	VisibleGroups []string `json:"visible_groups"`
}

// ClientRecord is one deployed client: its AWG key pair, overlay address,
// the BitTorrent peer ID the server uses to recognize its carrier handshake,
// the group it belongs to, and the hub-enforced routing policy applied to it.
type ClientRecord struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	PubKey       string    `json:"pubkey"`
	PrivKey      string    `json:"privkey"` // secret
	IP           string    `json:"ip"`
	PeerID       string    `json:"peer_id"`
	Created      time.Time `json:"created"`
	Revoked      bool      `json:"revoked"`
	NoInternet   bool      `json:"no_internet"`
	Internet     *bool     `json:"internet"`      // nil = inherit from group
	VisiblePeers []string  `json:"visible_peers"` // nil = inherit from group
	GroupID      string    `json:"group_id"`      // empty normalizes to DefaultGroupID
}

// Store wraps the bbolt database.
type Store struct {
	db *bolt.DB
}

// Open creates or opens the database at path. On the first open it creates
// the default group from the saved (or default) server configuration and
// migrates legacy client records into it.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketServer, bucketGroups, bucketClients, bucketSubscriptions} {
			if _, e := tx.CreateBucketIfNotExists(b); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init buckets: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate store: %w", err)
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// SaveServerConfig persists the hub configuration.
func (s *Store) SaveServerConfig(cfg config.ServerConfig) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketServer).Put(serverKey, b)
	})
}

// LoadServerConfig returns the saved hub configuration, or the shipped
// defaults when nothing has been saved yet.
func (s *Store) LoadServerConfig() (config.ServerConfig, error) {
	var cfg config.ServerConfig
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		cfg, err = s.loadServerConfigTx(tx)
		return err
	})
	return cfg, err
}

func (s *Store) loadServerConfigTx(tx *bolt.Tx) (config.ServerConfig, error) {
	var cfg config.ServerConfig
	raw := tx.Bucket(bucketServer).Get(serverKey)
	if raw == nil {
		cfg = config.DefaultServerConfig()
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// CreateClient stores a new client record. It rejects records whose IP or
// peer ID collides with any existing client (revoked or not).
//
// Callers that leave GroupID empty are treated as legacy clients: the record
// is placed in the default group and nil policy fields are materialized to
// explicit legacy ACLs.
func (s *Store) CreateClient(rec ClientRecord) error {
	NormalizeClientRecord(&rec)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketClients)
		var dupErr error
		b.ForEach(func(_, v []byte) error {
			var existing ClientRecord
			if json.Unmarshal(v, &existing) != nil {
				return nil
			}
			switch {
			case existing.IP == rec.IP:
				dupErr = ErrDuplicateIP
			case existing.PeerID == rec.PeerID:
				dupErr = ErrDuplicatePeerID
			}
			return nil
		})
		if dupErr != nil {
			return dupErr
		}
		return b.Put([]byte(rec.ID), mustJSON(rec))
	})
}

// ListClients returns all client records in insertion order.
func (s *Store) ListClients() ([]ClientRecord, error) {
	var recs []ClientRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketClients).ForEach(func(_, v []byte) error {
			var rec ClientRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			NormalizeClientRecord(&rec)
			recs = append(recs, rec)
			return nil
		})
	})
	return recs, err
}

// ClientRevocations returns the minimal client roster needed by live telemetry.
func (s *Store) ClientRevocations() (map[string]bool, error) {
	out := make(map[string]bool)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketClients).ForEach(func(key, raw []byte) error {
			var row struct {
				Revoked bool `json:"revoked"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			out[string(key)] = row.Revoked
			return nil
		})
	})
	return out, err
}

// GetClient returns one client record by ID.
func (s *Store) GetClient(id string) (ClientRecord, error) {
	var rec ClientRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketClients).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		NormalizeClientRecord(&rec)
		return nil
	})
	return rec, err
}

// RevokeClient marks a client revoked; its carrier is torn down by the caller.
func (s *Store) RevokeClient(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketClients)
		raw := b.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var rec ClientRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		NormalizeClientRecord(&rec)
		if !rec.Revoked {
			if err := invalidateEgressClientTx(tx, id); err != nil {
				return err
			}
		}
		rec.Revoked = true
		return b.Put([]byte(id), mustJSON(rec))
	})
}

// SaveClient persists an updated client record. It rejects unknown IDs and
// verifies that the client's group exists and its IP is consistent with the
// group subnet.
func (s *Store) SaveClient(rec ClientRecord) error {
	NormalizeClientRecord(&rec)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketClients)
		raw := b.Get([]byte(rec.ID))
		if raw == nil {
			return ErrNotFound
		}
		var previous ClientRecord
		if err := json.Unmarshal(raw, &previous); err != nil {
			return err
		}
		if previous.PubKey != rec.PubKey || previous.PrivKey != rec.PrivKey || previous.PeerID != rec.PeerID || !previous.Created.Equal(rec.Created) || previous.Revoked != rec.Revoked {
			if err := invalidateEgressClientTx(tx, rec.ID); err != nil {
				return err
			}
		}
		gRaw := tx.Bucket(bucketGroups).Get([]byte(rec.GroupID))
		if gRaw == nil {
			return ErrGroupNotFound
		}
		var g GroupRecord
		if err := json.Unmarshal(gRaw, &g); err != nil {
			return err
		}
		if err := validateClientIP(rec.IP, g); err != nil {
			return err
		}
		return b.Put([]byte(rec.ID), mustJSON(rec))
	})
}

// DeleteClient permanently removes a client record and its traffic history.
func (s *Store) DeleteClient(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		key := []byte(id)
		clients := tx.Bucket(bucketClients)
		if clients.Get(key) == nil {
			return ErrNotFound
		}
		if b := tx.Bucket(bucketTraffic); b != nil {
			if err := b.Delete(key); err != nil {
				return err
			}
		}
		if err := invalidateEgressClientTx(tx, id); err != nil {
			return err
		}
		if err := tx.Bucket(bucketSubscriptions).Delete(key); err != nil {
			return err
		}
		return clients.Delete(key)
	})
}

// ListGroups returns all group records in insertion order.
func (s *Store) ListGroups() ([]GroupRecord, error) {
	var recs []GroupRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketGroups).ForEach(func(_, v []byte) error {
			var rec GroupRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			recs = append(recs, rec)
			return nil
		})
	})
	return recs, err
}

// GetGroup returns one group record by ID.
func (s *Store) GetGroup(id string) (GroupRecord, error) {
	var rec GroupRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketGroups).Get([]byte(id))
		if raw == nil {
			return ErrGroupNotFound
		}
		return json.Unmarshal(raw, &rec)
	})
	return rec, err
}

// SaveGroup creates or updates a group record. It validates the subnet and
// rejects overlaps with other groups. If the subnet of an existing populated
// group is changed, all of its clients are renumbered atomically inside the
// same transaction; if reallocation fails the transaction is aborted and the
// previous state is left untouched.
func (s *Store) SaveGroup(rec GroupRecord) error {
	if rec.ID == "" {
		return ErrInvalidSubnet
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		oldRaw := b.Get([]byte(rec.ID))
		var renumber bool
		if oldRaw != nil {
			var old GroupRecord
			if err := json.Unmarshal(oldRaw, &old); err != nil {
				return err
			}
			if old.Subnet != rec.Subnet || old.ServerIP != rec.ServerIP {
				renumber = true
			}
		}
		if err := validateGroup(rec, tx); err != nil {
			return err
		}
		if renumber {
			if err := renumberGroupClients(tx, rec); err != nil {
				return err
			}
		}
		return b.Put([]byte(rec.ID), mustJSON(rec))
	})
}

// DeleteGroup removes an empty group. It rejects the default group and groups
// that still have clients.
func (s *Store) DeleteGroup(id string) error {
	if id == DefaultGroupID {
		return ErrDefaultGroup
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		if b.Get([]byte(id)) == nil {
			return ErrGroupNotFound
		}
		var count int
		cb := tx.Bucket(bucketClients)
		if err := cb.ForEach(func(_, v []byte) error {
			var c ClientRecord
			if err := json.Unmarshal(v, &c); err != nil {
				return err
			}
			if c.GroupID == id {
				count++
			}
			return nil
		}); err != nil {
			return err
		}
		if count > 0 {
			return ErrGroupNotEmpty
		}
		if err := invalidateEgressGroupTx(tx, id); err != nil {
			return err
		}
		return b.Delete([]byte(id))
	})
}

// AllocateClient stores a new client record, assigning an IP inside the
// specified group if rec.IP is empty. It validates that explicit IPs fall
// inside the group subnet and are not reserved, and atomically rejects
// collisions with any existing client.
func (s *Store) AllocateClient(rec ClientRecord) error {
	if rec.GroupID == "" {
		return ErrGroupNotFound
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		gb := tx.Bucket(bucketGroups)
		raw := gb.Get([]byte(rec.GroupID))
		if raw == nil {
			return ErrGroupNotFound
		}
		var g GroupRecord
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		prefix, err := netip.ParsePrefix(g.Subnet)
		if err != nil {
			return ErrInvalidSubnet
		}
		server, err := netip.ParseAddr(g.ServerIP)
		if err != nil {
			return ErrInvalidSubnet
		}

		used := make(map[netip.Addr]bool)
		used[prefix.Masked().Addr()] = true
		used[broadcast(prefix)] = true
		used[server] = true
		var dupErr error
		cb := tx.Bucket(bucketClients)
		if err := cb.ForEach(func(_, v []byte) error {
			var existing ClientRecord
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if a, err := netip.ParseAddr(existing.IP); err == nil {
				used[a] = true
			}
			if existing.PeerID == rec.PeerID {
				dupErr = ErrDuplicatePeerID
			}
			return nil
		}); err != nil {
			return err
		}
		if dupErr != nil {
			return dupErr
		}

		if rec.IP == "" {
			ip, err := allocateIP(prefix, server, used)
			if err != nil {
				return err
			}
			rec.IP = ip.String()
		} else {
			ip, err := netip.ParseAddr(rec.IP)
			if err != nil || !prefix.Contains(ip) {
				return ErrInvalidSubnet
			}
			if ip == prefix.Masked().Addr() || ip == broadcast(prefix) || ip == server {
				return ErrInvalidSubnet
			}
			if used[ip] {
				return ErrDuplicateIP
			}
		}
		return cb.Put([]byte(rec.ID), mustJSON(rec))
	})
}

// MoveClient moves a client to a different group and reallocates its IP
// inside the new group. The client's ID, keys, peer ID and policy fields are
// preserved.
func (s *Store) MoveClient(id, groupID string) error {
	if groupID == "" {
		return ErrGroupNotFound
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		cb := tx.Bucket(bucketClients)
		raw := cb.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var rec ClientRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		if rec.GroupID == groupID {
			return nil
		}
		gb := tx.Bucket(bucketGroups)
		gRaw := gb.Get([]byte(groupID))
		if gRaw == nil {
			return ErrGroupNotFound
		}
		var g GroupRecord
		if err := json.Unmarshal(gRaw, &g); err != nil {
			return err
		}
		prefix, err := netip.ParsePrefix(g.Subnet)
		if err != nil {
			return ErrInvalidSubnet
		}
		server, err := netip.ParseAddr(g.ServerIP)
		if err != nil {
			return ErrInvalidSubnet
		}
		used := make(map[netip.Addr]bool)
		used[prefix.Masked().Addr()] = true
		used[broadcast(prefix)] = true
		used[server] = true
		if err := cb.ForEach(func(_, v []byte) error {
			var existing ClientRecord
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.ID == id {
				return nil
			}
			if a, err := netip.ParseAddr(existing.IP); err == nil {
				used[a] = true
			}
			return nil
		}); err != nil {
			return err
		}
		ip, err := allocateIP(prefix, server, used)
		if err != nil {
			return err
		}
		rec.GroupID = groupID
		rec.IP = ip.String()
		return cb.Put([]byte(id), mustJSON(rec))
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// NormalizeClientRecord applies legacy defaults to records that predate the
// group fields. A record with an empty GroupID is placed in the default group
// and any nil policy fields are materialized to explicit legacy ACLs. Records
// that already have a non-empty GroupID are left untouched so that nil means
// "inherit from the group".
func NormalizeClientRecord(rec *ClientRecord) {
	if rec.GroupID == "" {
		rec.GroupID = DefaultGroupID
		if rec.VisiblePeers == nil {
			rec.VisiblePeers = []string{"*"}
		}
		if rec.Internet == nil {
			rec.Internet = new(!rec.NoInternet)
		}
	}
}

func (s *Store) migrate() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		gb := tx.Bucket(bucketGroups)
		var groupCount int
		if err := gb.ForEach(func(_, _ []byte) error { groupCount++; return nil }); err != nil {
			return err
		}
		if groupCount == 0 {
			cfg, err := s.loadServerConfigTx(tx)
			if err != nil {
				return err
			}
			g := GroupRecord{
				ID:       DefaultGroupID,
				Name:     "Default",
				Subnet:   cfg.Subnet,
				ServerIP: cfg.ServerIP,
			}
			if err := validateGroup(g, tx); err != nil {
				return fmt.Errorf("default group invalid: %w", err)
			}
			if err := gb.Put([]byte(g.ID), mustJSON(g)); err != nil {
				return err
			}
		}
		cb := tx.Bucket(bucketClients)
		return cb.ForEach(func(k, v []byte) error {
			var rec ClientRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if rec.GroupID != "" {
				return nil
			}
			rec.GroupID = DefaultGroupID
			if rec.VisiblePeers == nil {
				rec.VisiblePeers = []string{"*"}
			}
			rec.Internet = new(!rec.NoInternet)
			return cb.Put(k, mustJSON(rec))
		})
	})
}

func validateClientIP(ipStr string, g GroupRecord) error {
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return ErrInvalidSubnet
	}
	prefix, err := netip.ParsePrefix(g.Subnet)
	if err != nil {
		return ErrInvalidSubnet
	}
	server, err := netip.ParseAddr(g.ServerIP)
	if err != nil {
		return ErrInvalidSubnet
	}
	if !prefix.Contains(ip) || ip == prefix.Masked().Addr() || ip == broadcast(prefix) || ip == server {
		return ErrInvalidSubnet
	}
	return nil
}

func validateGroup(g GroupRecord, tx *bolt.Tx) error {
	prefix, err := netip.ParsePrefix(g.Subnet)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 || prefix.Bits() > 30 || prefix != prefix.Masked() {
		return ErrInvalidSubnet
	}
	server, err := netip.ParseAddr(g.ServerIP)
	if err != nil || !server.Is4() || !prefix.Contains(server) {
		return ErrInvalidSubnet
	}
	if server == prefix.Masked().Addr() || server == broadcast(prefix) {
		return ErrInvalidSubnet
	}
	b := tx.Bucket(bucketGroups)
	return b.ForEach(func(k, v []byte) error {
		if string(k) == g.ID {
			return nil
		}
		var other GroupRecord
		if err := json.Unmarshal(v, &other); err != nil {
			return err
		}
		op, err := netip.ParsePrefix(other.Subnet)
		if err != nil {
			return nil
		}
		if prefixesOverlap(prefix, op) {
			return ErrSubnetOverlap
		}
		return nil
	})
}

func renumberGroupClients(tx *bolt.Tx, g GroupRecord) error {
	prefix, err := netip.ParsePrefix(g.Subnet)
	if err != nil {
		return ErrInvalidSubnet
	}
	server, err := netip.ParseAddr(g.ServerIP)
	if err != nil {
		return ErrInvalidSubnet
	}
	cb := tx.Bucket(bucketClients)
	var clients []ClientRecord
	if err := cb.ForEach(func(_, v []byte) error {
		var c ClientRecord
		if err := json.Unmarshal(v, &c); err != nil {
			return err
		}
		if c.GroupID == g.ID {
			clients = append(clients, c)
		}
		return nil
	}); err != nil {
		return err
	}
	if len(clients) == 0 {
		return nil
	}
	used := make(map[netip.Addr]bool)
	used[prefix.Masked().Addr()] = true
	used[broadcast(prefix)] = true
	used[server] = true
	for i := range clients {
		ip, err := allocateIP(prefix, server, used)
		if err != nil {
			return err
		}
		clients[i].IP = ip.String()
		used[ip] = true
	}
	for _, c := range clients {
		if err := cb.Put([]byte(c.ID), mustJSON(c)); err != nil {
			return err
		}
	}
	return nil
}

func allocateIP(prefix netip.Prefix, server netip.Addr, used map[netip.Addr]bool) (netip.Addr, error) {
	start := prefix.Masked().Addr()
	end := broadcast(prefix)
	for addr := nextAddr(start); addr.Compare(end) < 0; addr = nextAddr(addr) {
		if addr == server || used[addr] {
			continue
		}
		return addr, nil
	}
	return netip.Addr{}, ErrSubnetExhausted
}

func nextAddr(addr netip.Addr) netip.Addr {
	b := addr.As4()
	u := binary.BigEndian.Uint32(b[:]) + 1
	binary.BigEndian.PutUint32(b[:], u)
	return netip.AddrFrom4(b)
}

func broadcast(prefix netip.Prefix) netip.Addr {
	b := prefix.Masked().Addr().As4()
	hostBits := 32 - prefix.Bits()
	if hostBits >= 32 {
		return netip.AddrFrom4(b)
	}
	inc := uint32(1) << hostBits
	u := binary.BigEndian.Uint32(b[:]) + inc - 1
	binary.BigEndian.PutUint32(b[:], u)
	return netip.AddrFrom4(b)
}

func prefixesOverlap(a, b netip.Prefix) bool {
	return a.Contains(b.Masked().Addr()) || b.Contains(a.Masked().Addr())
}
