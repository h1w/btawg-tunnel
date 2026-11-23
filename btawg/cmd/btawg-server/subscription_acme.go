package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"

	"bittorrent-tunnel/btawg/internal/store"
)

const (
	subscriptionRenewBefore = 48 * time.Hour
	subscriptionRetryMin    = 5 * time.Minute
	subscriptionRetryMax    = time.Hour
)

var errSubscriptionCertUnavailable = errors.New("no valid subscription certificate")

type subscriptionCertificates struct {
	host    string
	store   *store.Store
	current atomic.Pointer[tls.Certificate]
	failure atomic.Pointer[string]
}

func validSubscriptionCertificate(pair *tls.Certificate, host string, now time.Time) error {
	if pair == nil || pair.Leaf == nil || now.Before(pair.Leaf.NotBefore) || !now.Before(pair.Leaf.NotAfter) {
		return errSubscriptionCertUnavailable
	}
	return pair.Leaf.VerifyHostname(host)
}

func (m *subscriptionCertificates) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	pair := m.current.Load()
	if pair == nil || !time.Now().Before(pair.Leaf.NotAfter) {
		return nil, errSubscriptionCertUnavailable
	}
	return pair, nil
}

func (m *subscriptionCertificates) restore() error {
	data, err := m.store.LoadSubscriptionCertificate(m.host)
	if err != nil || len(data) == 0 {
		return err
	}
	pair, err := tls.X509KeyPair(data, data)
	if err != nil {
		return err
	}
	if err := validSubscriptionCertificate(&pair, m.host, time.Now()); err != nil {
		return err
	}
	m.current.Store(&pair)
	return nil
}

func (m *subscriptionCertificates) replace(pair *tls.Certificate) error {
	if err := validSubscriptionCertificate(pair, m.host, time.Now()); err != nil {
		return err
	}
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		return err
	}
	var data bytes.Buffer
	for _, der := range pair.Certificate {
		_ = pem.Encode(&data, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	_ = pem.Encode(&data, &pem.Block{Type: "PRIVATE KEY", Bytes: key})
	if err := m.store.SaveSubscriptionCertificate(m.host, data.Bytes()); err != nil {
		return err
	}
	m.current.Store(pair)
	return nil
}

func (m *subscriptionCertificates) status() (bool, string) {
	_, err := m.getCertificate(nil)
	ready := err == nil
	if failure := m.failure.Load(); failure != nil {
		if ready {
			return true, "Certificate renewal failed: " + *failure + " The current certificate is still valid; the server will retry."
		}
		return false, "Certificate issuance failed: " + *failure + " The server will retry."
	}
	if ready {
		return true, ""
	}
	return false, "Requesting a Let's Encrypt certificate. Allow inbound TCP 80 for validation and TCP 2228 for downloads, then reload this page."
}

func (m *subscriptionCertificates) renew(ctx context.Context, issue func(context.Context) (*tls.Certificate, error)) error {
	pair, err := issue(ctx)
	if err == nil {
		err = m.replace(pair)
	}
	if err == nil {
		m.failure.Store(nil)
	} else if !errors.Is(ctx.Err(), context.Canceled) {
		var port *net.OpError
		message := "Check that the configured public address reaches this hub on TCP 80; see the hub log for details."
		if errors.As(err, &port) && port.Op == "listen" {
			message = "Could not listen on TCP 80. Check that the port is free and the hub can bind it."
		}
		m.failure.Store(&message)
	}
	return err
}

func (m *subscriptionCertificates) run(ctx context.Context, directory string, listen func(context.Context) (net.Listener, error)) {
	var client *acme.Client
	issue := func(ctx context.Context) (*tls.Certificate, error) {
		if client == nil {
			key, err := subscriptionAccount(m.store)
			if err != nil {
				return nil, err
			}
			client = &acme.Client{Key: key, DirectoryURL: directory, RetryBackoff: func(n int, _ *http.Request, response *http.Response) time.Duration {
				// Let the renewal loop honor the CA's Retry-After on rate limits.
				if response.StatusCode == http.StatusTooManyRequests || n > 3 {
					return 0
				}
				return time.Duration(n) * time.Second
			}}
		}
		return issueSubscriptionCert(ctx, client, m.host, listen)
	}
	retry := subscriptionRetryMin
	var delay time.Duration
	if cert := m.current.Load(); cert != nil {
		delay = max(0, time.Until(cert.Leaf.NotAfter.Add(-subscriptionRenewBefore)))
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		attempt, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := m.renew(attempt, issue)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("btawg-server: subscription ACME: %v", err)
			delay = retry
			retry = min(2*retry, subscriptionRetryMax)
			if acmeErr, ok := errors.AsType[*acme.Error](err); ok {
				if after, limited := acme.RateLimit(acmeErr); limited {
					delay = max(after, delay)
				}
			}
		} else {
			current := m.current.Load()
			log.Printf("btawg-server: subscription certificate for %s valid until %s", m.host, current.Leaf.NotAfter.Format(time.RFC3339))
			delay = max(subscriptionRetryMin, time.Until(current.Leaf.NotAfter.Add(-subscriptionRenewBefore)))
			retry = subscriptionRetryMin
		}
		timer.Reset(delay)
	}
}

func subscriptionAccount(st *store.Store) (*ecdsa.PrivateKey, error) {
	der, err := st.LoadSubscriptionAccount()
	if err != nil {
		return nil, err
	}
	if len(der) != 0 {
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("read ACME account key: %w", err)
		}
		if key, ok := key.(*ecdsa.PrivateKey); ok {
			return key, nil
		}
		return nil, errors.New("invalid stored ACME account key type")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err = x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return key, st.SaveSubscriptionAccount(der)
}

func issueSubscriptionCert(ctx context.Context, client *acme.Client, ip string, listen func(context.Context) (net.Listener, error)) (*tls.Certificate, error) {
	if client.KID == "" {
		if _, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
			return nil, fmt.Errorf("register ACME account: %w", err)
		}
	}
	order, err := client.AuthorizeOrder(ctx, acme.IPIDs(ip), acme.WithOrderIssuanceProfile("shortlived"))
	if err != nil {
		return nil, fmt.Errorf("order IP certificate: %w", err)
	}
	for _, url := range order.AuthzURLs {
		auth, err := client.GetAuthorization(ctx, url)
		if err != nil {
			return nil, err
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		if err := authorizeSubscriptionIP(ctx, client, auth, listen); err != nil {
			return nil, err
		}
	}
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{IPAddresses: []net.IP{net.ParseIP(ip)}}, key)
	if err != nil {
		return nil, err
	}
	chain, _, err := client.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize IP certificate: %w", err)
	}
	if len(chain) == 0 {
		return nil, errors.New("ACME returned an empty certificate chain")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	public, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !key.PublicKey.Equal(public) {
		return nil, errors.New("ACME certificate does not match the requested public key")
	}
	return &tls.Certificate{Certificate: chain, Leaf: leaf, PrivateKey: key}, nil
}

func authorizeSubscriptionIP(ctx context.Context, client *acme.Client, auth *acme.Authorization, listen func(context.Context) (net.Listener, error)) error {
	for _, challenge := range auth.Challenges {
		if challenge.Type != "http-01" {
			continue
		}
		path := client.HTTP01ChallengePath(challenge.Token)
		value, err := client.HTTP01ChallengeResponse(challenge.Token)
		if err != nil {
			return err
		}
		ln, err := listen(ctx)
		if err != nil {
			return fmt.Errorf("ACME HTTP-01: %w", err)
		}
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 4096, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != path {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, value)
		})}
		defer ln.Close()
		defer server.Close()
		go func() { _ = server.Serve(ln) }()
		if _, err := client.Accept(ctx, challenge); err != nil {
			return fmt.Errorf("accept IP challenge: %w", err)
		}
		_, err = client.WaitAuthorization(ctx, auth.URI)
		return err
	}
	return errors.New("ACME did not offer an HTTP-01 challenge for this IP")
}
