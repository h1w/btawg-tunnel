package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/service"
)

// A binary update can change the import version. Keep the saved configuration
// usable by the old service until an explicit subscription refresh replaces it.
// Changes to either the enrollment or its subscription invalidate the sidecar.
type updateProfile struct {
	Enrollment   [sha256.Size]byte `json:"enrollment"`
	Subscription [sha256.Size]byte `json:"subscription"`
	Import       string            `json:"import"`
}

func (c *ManagedConfig) updateProfileKey() updateProfile {
	return updateProfile{Enrollment: sha256.Sum256([]byte(c.Import)), Subscription: sha256.Sum256([]byte(c.SubscriptionURL))}
}

// Each installed Windows bundle has its own immutable directory. Keep secrets
// under the managed data ACL, not in Program Files or in the update journal.
func updateProfilePath(configPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	bundle := filepath.Dir(exe)
	if runtime.GOOS == "windows" {
		bundle = strings.ToLower(bundle)
	}
	id := sha256.Sum256([]byte(bundle))
	return configPath + ".update-" + hex.EncodeToString(id[:]), nil
}

// StageUpdateProfile persists the authenticated subscription response separately
// from client.json; a failed activation or an interrupted rollback needs no undo.
func (c *ManagedConfig) StageUpdateProfile(path, raw string) error {
	p := c.updateProfileKey()
	p.Import = raw
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	file, err := updateProfilePath(path)
	if err != nil {
		return err
	}
	return service.WriteManagedFile(file, data)
}

// RuntimeProfile resolves the enrolled profile without modifying it or contacting
// the network. It never makes an old import version runnable by itself.
func (c *ManagedConfig) RuntimeProfile(path string) (config.ClientConfig, error) {
	cfg, err := config.ParseImportString(c.Import)
	if err == nil || cfg.Version != 1 || c.SubscriptionURL == "" {
		return cfg, err
	}
	file, pathErr := updateProfilePath(path)
	if pathErr != nil {
		return cfg, pathErr
	}
	data, readErr := os.ReadFile(file)
	if errors.Is(readErr, os.ErrNotExist) {
		return cfg, err
	}
	if readErr != nil {
		return cfg, readErr
	}
	var p updateProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return cfg, err
	}
	key := c.updateProfileKey()
	if p.Enrollment != key.Enrollment || p.Subscription != key.Subscription {
		return cfg, err
	}
	return config.ParseImportString(p.Import)
}
