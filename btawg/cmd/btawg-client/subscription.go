package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/service"
)

const maxSubscriptionProfile = 64 << 10

var subscriptionHTTP = &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{
	Timeout: 10 * time.Second,
}).DialContext, DisableKeepAlives: true}}

type vpnImport struct {
	profile string
	url     string
	mode    string
}

func resolveVPNImport(input string, previous *client.ManagedConfig) (vpnImport, error) {
	input = strings.TrimSpace(input)
	if input == "" && previous != nil {
		return vpnImport{}, nil
	}
	if strings.HasPrefix(input, config.ImportPrefix) {
		profile, err := config.ParseImportString(input)
		if err != nil {
			return vpnImport{}, errors.New("invalid VPN import: paste the complete btawg:// string from the server panel")
		}
		mode := config.CanonicalMode(profile.Transport)
		if !config.ValidClientMode(mode) {
			return vpnImport{}, errors.New("VPN import uses an unsupported transport")
		}
		return vpnImport{profile: input, mode: mode}, nil
	}
	return fetchSubscription(context.Background(), subscriptionHTTP, input, previous)
}

func fetchSubscription(ctx context.Context, hc *http.Client, link string, local *client.ManagedConfig) (vpnImport, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return vpnImport{}, errors.New("expected a btawg:// import or an HTTPS subscription link")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return vpnImport{}, errors.New("invalid HTTPS subscription link")
	}
	req.Header.Set("Accept", "text/plain")
	fetch := *hc
	fetch.Jar = nil
	fetch.Timeout = 10 * time.Second
	fetch.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r, err := fetch.Do(req)
	if err != nil {
		// Even a wrapped transport error can quote a malformed redirect's
		// Location, which can contain the bearer path. Never persist it.
		return vpnImport{}, errors.New("subscription HTTPS request failed")
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return vpnImport{}, fmt.Errorf("subscription server returned HTTP %d", r.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSubscriptionProfile+1))
	if err != nil {
		return vpnImport{}, errors.New("could not read subscription profile")
	}
	if len(body) > maxSubscriptionProfile {
		return vpnImport{}, errors.New("subscription profile is too large")
	}
	raw := strings.TrimSpace(string(body))
	cfg, err := config.ParseImportString(raw)
	if err != nil || strings.ContainsAny(raw, "\r\n") || !validSubscriptionProfile(cfg, local) {
		return vpnImport{}, errors.New("subscription returned an invalid btawg profile for the selected transports")
	}
	return vpnImport{profile: raw, url: link, mode: config.CanonicalMode(cfg.Transport)}, nil
}

func validSubscriptionProfile(cfg config.ClientConfig, local *client.ManagedConfig) bool {
	validHex := func(v string, n int) bool {
		if len(v) != n*2 {
			return false
		}
		var buf [32]byte
		_, err := hex.Decode(buf[:n], []byte(v))
		return err == nil
	}
	validEndpoint := func(v string, carrier bool) bool {
		if v == "" {
			return true
		}
		host, port, err := net.SplitHostPort(v)
		if err != nil || host == "" {
			return false
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return false
		}
		if !carrier {
			return true
		}
		a, err := netip.ParseAddr(host)
		return err == nil && a.Is4() && !a.IsUnspecified()
	}
	clientIP, _ := netip.ParseAddr(cfg.ClientIP)
	serverIP, _ := netip.ParseAddr(cfg.ServerIP)
	subnet, _ := netip.ParsePrefix(cfg.Subnet)
	mode := config.CanonicalMode(cfg.Transport)
	if !clientIP.Is4() || !serverIP.Is4() || clientIP == serverIP || subnet.Bits() <= 0 || !subnet.Contains(clientIP) || !subnet.Contains(serverIP) ||
		!config.ValidClientMode(mode) || cfg.Profile.Validate() != nil ||
		!validHex(cfg.ServerPubKey, 32) || !validHex(cfg.ClientPrivKey, 32) || !validHex(cfg.ClientPubKey, 32) ||
		cfg.TLSServerKey != "" && !validHex(cfg.TLSServerKey, 32) ||
		!validEndpoint(cfg.CarrierEndpoint, true) || !validEndpoint(cfg.AWGEndpoint, false) || !validEndpoint(cfg.WGEndpoint, false) {
		return false
	}
	for _, raw := range cfg.Routes {
		p, err := netip.ParsePrefix(raw)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			return false
		}
	}
	for _, hash := range cfg.CarrierHashes {
		if !validHex(hash, 20) {
			return false
		}
	}
	if cfg.InfoHash != "" && !validHex(cfg.InfoHash, 20) {
		return false
	}
	usable := func(mode string) bool {
		switch mode {
		case config.ModeAWG:
			return cfg.AWGEndpoint != ""
		case config.ModeWG:
			return cfg.WGEndpoint != ""
		}
		if !config.IsCarrierMode(mode) || config.IsTLSMode(mode) && cfg.TLSServerKey == "" {
			return false
		}
		if cfg.CarrierHashes[mode] != "" || !config.IsTLSMode(mode) && cfg.InfoHash != "" {
			return true
		}
		for name := range cfg.CarrierHashes {
			if config.CanonicalMode(name) == mode {
				return true
			}
		}
		return false
	}
	if local == nil {
		return usable(mode)
	}
	for _, enabled := range local.Enabled {
		if !usable(enabled) {
			return false
		}
	}
	for _, pool := range local.Balancers {
		for _, mode := range pool {
			if !usable(mode) {
				return false
			}
		}
	}
	return true
}

// prepareUpdateProfile runs only during an explicitly requested Windows update.
// Do not stop a working v1 service if its replacement profile cannot be fetched.
func prepareUpdateProfile(ctx context.Context, hc *http.Client, path string) error {
	local, err := client.LoadManagedConfig(path)
	if err != nil {
		return err
	}
	profile, err := config.ParseImportString(local.Import)
	if err == nil || profile.Version != 1 {
		return err
	}
	if local.SubscriptionURL == "" {
		return errors.New("this update requires a version-2 profile; configure an HTTPS subscription before updating, or install with a fresh import")
	}
	resolved, err := fetchSubscription(ctx, hc, local.SubscriptionURL, &local)
	if err != nil {
		return err
	}
	latest, err := client.LoadManagedConfig(path)
	if err != nil {
		return err
	}
	if latest.Import != local.Import || latest.SubscriptionURL != local.SubscriptionURL {
		return errors.New("enrollment changed during update preparation")
	}
	current, err := config.ParseImportString(resolved.profile)
	if err != nil || !validSubscriptionProfile(current, &latest) {
		return errors.New("subscription profile does not support the current transport selection")
	}
	return latest.StageUpdateProfile(path, resolved.profile)
}

func refreshSubscription(ctx context.Context, hc *http.Client, path string, apply func(func() error) error) (bool, error) {
	previous, err := client.LoadManagedConfig(path)
	if err != nil {
		return false, err
	}
	if previous.SubscriptionURL == "" {
		return false, errors.New("set an HTTPS subscription link in Service settings first")
	}
	resolved, err := fetchSubscription(ctx, hc, previous.SubscriptionURL, &previous)
	if err != nil || resolved.profile == previous.Import {
		return false, err
	}
	changed := false
	err = apply(func() error {
		latest, err := client.LoadManagedConfig(path)
		if err != nil {
			return err
		}
		if latest.SubscriptionURL != previous.SubscriptionURL || latest.Import != previous.Import {
			return errors.New("VPN import changed during refresh; try again")
		}
		cfg, _ := config.ParseImportString(resolved.profile)
		if !validSubscriptionProfile(cfg, &latest) {
			return errors.New("subscription profile does not support the current transport selection")
		}
		latest.Import = resolved.profile
		err = latest.Save(path)
		changed = err == nil
		return err
	})
	return changed, err
}

func refreshSubscriptionScreen() (*service.Screen, error) {
	changed, err := refreshSubscription(context.Background(), subscriptionHTTP, client.ManagedConfigPath, persistSubscriptionProfile)
	if err != nil {
		if changed {
			return nil, fmt.Errorf("profile saved; restart the client service manually: %w", err)
		}
		return nil, err
	}
	text := "The profile is already up to date. The service was not restarted."
	if changed {
		text = "The updated VPN profile was saved. If the service was running, it was restarted."
	}
	return &service.Screen{Title: "Subscription", Text: text}, nil
}
