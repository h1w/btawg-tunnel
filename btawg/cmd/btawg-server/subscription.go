package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/panel"
)

type subscriptionSettings struct {
	SubscriptionHost string `json:"subscription_host,omitempty"`
	SubscriptionCert string `json:"subscription_cert,omitempty"`
	SubscriptionKey  string `json:"subscription_key,omitempty"`
}

func (s *subscriptionSettings) flags(fs *flag.FlagSet) {
	fs.StringVar(&s.SubscriptionHost, "subscription-host", "", "override the public subscription host (default: hub public IPv4; TCP 2228)")
	fs.StringVar(&s.SubscriptionCert, "subscription-cert", "", "subscription certificate chain PEM file")
	fs.StringVar(&s.SubscriptionKey, "subscription-key", "", "subscription private key PEM file")
}

func (s *subscriptionSettings) override(name string, source subscriptionSettings) {
	switch name {
	case "subscription-host":
		s.SubscriptionHost = source.SubscriptionHost
	case "subscription-cert":
		s.SubscriptionCert = source.SubscriptionCert
	case "subscription-key":
		s.SubscriptionKey = source.SubscriptionKey
	}
}

func (s *subscriptionSettings) overrideFlags(fs *flag.FlagSet, source subscriptionSettings) {
	fs.Visit(func(f *flag.Flag) { s.override(f.Name, source) })
}

func (s subscriptionSettings) loadTLS() (*tls.Config, error) {
	if s == (subscriptionSettings{}) {
		return nil, nil
	}
	if s.SubscriptionCert == "" || s.SubscriptionKey == "" {
		return nil, errors.New("subscriptions require a certificate PEM and private key PEM")
	}
	if s.SubscriptionHost != "" && !validSubscriptionHost(s.SubscriptionHost) {
		return nil, errors.New("invalid subscription host: use a DNS name or IP address without a scheme or port")
	}
	cert, err := tls.LoadX509KeyPair(s.SubscriptionCert, s.SubscriptionKey)
	if err != nil {
		return nil, fmt.Errorf("subscription TLS: %w", err)
	}
	if s.SubscriptionHost != "" {
		if err := cert.Leaf.VerifyHostname(s.SubscriptionHost); err != nil {
			return nil, fmt.Errorf("subscription TLS host: %w", err)
		}
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// publicHost resolves the default only once the hub's active address is known.
// An explicit override has already been checked by loadTLS.
func (s subscriptionSettings) publicHost(externalIP string, tlsConfig *tls.Config) (string, error) {
	if s.SubscriptionHost != "" {
		return s.SubscriptionHost, nil
	}
	ip, err := netip.ParseAddr(externalIP)
	if err != nil || !config.PublicIPv4(ip) {
		return "", errors.New("subscriptions need the hub's public IPv4; set ExternalIP or -subscription-host")
	}
	if err := tlsConfig.Certificates[0].Leaf.VerifyHostname(externalIP); err != nil {
		return "", fmt.Errorf("subscription certificate must include the hub IP in its IP SAN: %w", err)
	}
	return externalIP, nil
}

func validSubscriptionHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == "" && !ip.IsUnspecified()
	}
	if len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			b := label[i]
			if b != '-' && !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9') {
				return false
			}
		}
	}
	return true
}

func startSubscription(ctx context.Context, p *panel.Panel, host string, tlsConfig *tls.Config) (*http.Server, <-chan error, error) {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(panel.SubscriptionPort))
	if err != nil {
		return nil, nil, fmt.Errorf("subscription listener: %w", err)
	}
	web := &http.Server{
		Handler: p.SubscriptionHandler(host), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096,
	}
	errs := make(chan error, 1)
	go func() {
		defer ln.Close()
		if err := web.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("subscription HTTPS: %w", err)
		}
	}()
	return web, errs, nil
}
