package client

import (
	"errors"
	"fmt"

	"bittorrent-tunnel/btawg/internal/service"
)

// RequestControl reads or updates proxy settings through the daemon when it is
// running, or edits the persistent configuration while the service is stopped.
func RequestControl(req ControlRequest) (r ControlResponse, running bool, err error) {
	r, err = request(ControlSocketPath, req)
	if !errors.Is(err, ErrNotRunning) {
		return r, true, err
	}
	if req.mutates() {
		unlock, err := lockOfflineConfig()
		if err != nil {
			return r, false, err
		}
		defer unlock()
		state, _, err := service.Client.State()
		if err != nil {
			return r, false, err
		}
		if state != "inactive" && state != "failed" && state != service.NotInstalled {
			return r, false, fmt.Errorf("service is %s; control socket is not ready", state)
		}
	}
	cfg, err := LoadManagedConfig(ManagedConfigPath)
	if err != nil {
		return r, false, err
	}
	if err := cfg.update(req, nil, false); err != nil {
		return r, false, err
	}
	if req.mutates() {
		if err := cfg.Save(ManagedConfigPath); err != nil {
			return r, false, err
		}
	}
	return cfg.response(req.Op), false, nil
}
