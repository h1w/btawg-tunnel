package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/panel"
	"bittorrent-tunnel/btawg/internal/store"
)

type subscriptionSettings struct {
	SubscriptionHost string `json:"subscription_host,omitempty"`
	SubscriptionCert string `json:"subscription_cert,omitempty"`
	SubscriptionKey  string `json:"subscription_key,omitempty"`
	SubscriptionAuto *bool  `json:"subscription_auto,omitempty"`
}

func (s *subscriptionSettings) flags(fs *flag.FlagSet) {
	fs.StringVar(&s.SubscriptionHost, "subscription-host", "", "override the public subscription host (default: hub public IPv4; TCP 2228)")
	fs.StringVar(&s.SubscriptionCert, "subscription-cert", "", "subscription certificate chain PEM file")
	fs.StringVar(&s.SubscriptionKey, "subscription-key", "", "subscription private key PEM file")
	s.SubscriptionAuto = fs.Bool("subscription-auto", true, "automatically issue and renew the public IP certificate (default for managed hub)")
}

func (s *subscriptionSettings) override(name string, source subscriptionSettings) {
	switch name {
	case "subscription-host":
		s.SubscriptionHost = source.SubscriptionHost
	case "subscription-cert":
		s.SubscriptionCert = source.SubscriptionCert
	case "subscription-key":
		s.SubscriptionKey = source.SubscriptionKey
	case "subscription-auto":
		s.SubscriptionAuto = source.SubscriptionAuto
	}
}

func (s *subscriptionSettings) overrideFlags(fs *flag.FlagSet, source subscriptionSettings) {
	fs.Visit(func(f *flag.Flag) { s.override(f.Name, source) })
}

func (s subscriptionSettings) loadTLS() (*tls.Config, error) {
	if s.SubscriptionHost != "" && !validSubscriptionHost(s.SubscriptionHost) {
		return nil, errors.New("invalid subscription host: use a DNS name or IP address without a scheme or port")
	}
	if s.SubscriptionCert == "" && s.SubscriptionKey == "" {
		return nil, nil
	}
	if s.SubscriptionCert == "" || s.SubscriptionKey == "" {
		return nil, errors.New("subscriptions require a certificate PEM and private key PEM")
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

func (s subscriptionSettings) automatic(managed bool) bool {
	if s.SubscriptionAuto != nil {
		return *s.SubscriptionAuto
	}
	return managed
}

// serveSubscriptions configures the admin panel and owns the independent TLS
// listener. No error on this path should stop the hub or its VPN listeners.
func serveSubscriptions(ctx context.Context, p *panel.Panel, st *store.Store, s subscriptionSettings, externalIP string, managed bool) func() {
	failed := func(err error) func() {
		log.Printf("btawg-server: subscription HTTPS: %v", err)
		p.SubscriptionHandler("", func() (bool, string) { return false, err.Error() })
		return nil
	}
	tlsConfig, err := s.loadTLS()
	if err != nil {
		return failed(err)
	}
	var certificates *subscriptionCertificates
	var host string
	var readiness func() (bool, string)
	if tlsConfig != nil {
		host, err = s.publicHost(externalIP, tlsConfig)
		if err == nil {
			err = validSubscriptionCertificate(&tlsConfig.Certificates[0], host, time.Now())
		}
		if err != nil {
			return failed(err)
		}
		readiness = func() (bool, string) {
			if err := validSubscriptionCertificate(&tlsConfig.Certificates[0], host, time.Now()); err != nil {
				return false, "The configured subscription certificate is no longer valid. Replace the PEM files and restart the hub."
			}
			return true, ""
		}
	} else {
		if !s.automatic(managed) {
			return nil
		}
		host = s.SubscriptionHost
		if host == "" {
			host = externalIP
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is4() || !config.PublicIPv4(ip) {
			return failed(errors.New("Automatic subscription HTTPS needs a public IPv4. Set External IP in the server panel and restart the hub. For a DNS hostname, configure PEM files."))
		}
		certificates = &subscriptionCertificates{host: host, store: st}
		if err := certificates.restore(); err != nil {
			log.Printf("btawg-server: ignored cached subscription certificate: %v", err)
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certificates.getCertificate}
		readiness = certificates.status
	}

	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(panel.SubscriptionPort))
	if err != nil {
		return failed(fmt.Errorf("could not listen on subscription TCP 2228: %w", err))
	}
	var stopped atomic.Bool
	web := &http.Server{
		Handler: p.SubscriptionHandler(host, func() (bool, string) {
			if stopped.Load() {
				return false, "The subscription listener stopped. See the hub log for details."
			}
			return readiness()
		}), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096,
	}
	go func() {
		defer ln.Close()
		if err := web.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
			stopped.Store(true)
			log.Printf("btawg-server: subscription HTTPS: %v", err)
		}
	}()
	log.Printf("btawg-server: subscription listener on TCP %d for %s", panel.SubscriptionPort, host)
	if certificates == nil {
		return func() { _ = web.Close() }
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		certificates.run(ctx, "", func(ctx context.Context) (net.Listener, error) {
			return lc.Listen(ctx, "tcp4", ":80")
		})
	}()
	return func() { cancel(); _ = web.Close(); <-done }
}
