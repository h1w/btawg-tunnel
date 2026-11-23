package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/store"
	"golang.org/x/crypto/acme"
)

func TestSubscriptionACMEIPOrder(t *testing.T) {
	const ip = "192.0.2.15"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var nonce atomic.Uint64
	var der []byte
	var orderAccepted, challengeAccepted bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		base := "http://" + r.Host
		w.Header().Set("Replay-Nonce", base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprint(nonce.Add(1)))))
		write := func(status int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		decode := func(dst any) bool {
			var jws struct{ Payload string }
			if err := json.NewDecoder(r.Body).Decode(&jws); err != nil {
				t.Error(err)
				return false
			}
			b, err := base64.RawURLEncoding.DecodeString(jws.Payload)
			if err != nil || json.Unmarshal(b, dst) != nil {
				t.Error("invalid ACME JWS payload")
				return false
			}
			return true
		}
		switch r.URL.Path {
		case "/directory":
			write(200, map[string]any{"newNonce": base + "/nonce", "newAccount": base + "/account", "newOrder": base + "/new-order", "meta": map[string]any{"termsOfService": base + "/terms", "profiles": map[string]string{"shortlived": "IP certificates"}}})
		case "/nonce":
			w.WriteHeader(200)
		case "/account":
			var req struct{ TermsOfServiceAgreed bool }
			if !decode(&req) || !req.TermsOfServiceAgreed {
				t.Error("account did not agree to ACME terms")
				http.Error(w, "terms required", 400)
				return
			}
			w.Header().Set("Location", base+"/account/1")
			write(201, map[string]string{"status": "valid"})
		case "/new-order":
			var req struct {
				Profile     string
				Identifiers []acme.AuthzID
			}
			if !decode(&req) || req.Profile != "shortlived" || len(req.Identifiers) != 1 || req.Identifiers[0] != (acme.AuthzID{Type: "ip", Value: ip}) {
				t.Error("order did not use the shortlived IP identifier")
				http.Error(w, "wrong order", 400)
				return
			}
			orderAccepted = true
			w.Header().Set("Location", base+"/order")
			write(201, map[string]any{"status": "pending", "authorizations": []string{base + "/authz"}, "finalize": base + "/finalize"})
		case "/authz":
			status := "pending"
			if challengeAccepted {
				status = "valid"
			}
			write(200, map[string]any{"status": status, "identifier": acme.AuthzID{Type: "ip", Value: ip}, "challenges": []map[string]string{{"type": "http-01", "token": "some-token", "url": base + "/challenge"}}})
		case "/challenge":
			for path, want := range map[string]int{"/.well-known/acme-challenge/some-token": 200, "/.well-known/acme-challenge/wrong": 404, "/sub/some-token": 404, "/login": 404} {
				response, err := http.Get("http://" + ln.Addr().String() + path)
				if err != nil {
					t.Error(err)
					http.Error(w, "challenge unreachable", 400)
					return
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				client := acme.Client{Key: key}
				wantBody, _ := client.HTTP01ChallengeResponse("some-token")
				if response.StatusCode != want || want == 200 && string(body) != wantBody {
					t.Errorf("challenge request %s: status %d", path, response.StatusCode)
				}
			}
			challengeAccepted = true
			write(200, map[string]string{"status": "processing"})
		case "/order":
			write(200, map[string]string{"status": "ready", "finalize": base + "/finalize"})
		case "/finalize":
			var req struct{ CSR string }
			if !decode(&req) {
				http.Error(w, "missing CSR", 400)
				return
			}
			b, _ := base64.RawURLEncoding.DecodeString(req.CSR)
			csr, err := x509.ParseCertificateRequest(b)
			if err != nil || csr.CheckSignature() != nil || len(csr.IPAddresses) != 1 || csr.IPAddresses[0].String() != ip || len(csr.DNSNames) != 0 {
				t.Error("CSR did not request the IP SAN")
				http.Error(w, "invalid CSR", 400)
				return
			}
			der = testSubscriptionLeaf(t, csr.PublicKey, key, ip, time.Now().Add(6*24*time.Hour), 1)
			write(200, map[string]string{"status": "valid", "certificate": base + "/cert"})
		case "/cert":
			_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &acme.Client{DirectoryURL: srv.URL + "/directory", Key: key}
	pair, err := issueSubscriptionCert(ctx, client, ip, func(context.Context) (net.Listener, error) { return ln, nil })
	mu.Lock()
	defer mu.Unlock()
	if err != nil || !orderAccepted || !challengeAccepted {
		t.Fatalf("ACME issuance did not complete: %v", err)
	}
	if err := pair.Leaf.VerifyHostname(ip); err != nil {
		t.Fatal(err)
	}
}

func testSubscriptionLeaf(t *testing.T, public any, signer *ecdsa.PrivateKey, ip string, expires time.Time, serial int64) []byte {
	t.Helper()
	cert := &x509.Certificate{SerialNumber: big.NewInt(serial), IPAddresses: []net.IP{net.ParseIP(ip)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expires}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, signer)
	if err != nil {
		t.Error(err)
	}
	return der
}

func TestSubscriptionCertificateRenewalRetainsWorkingTLS(t *testing.T) {
	const ip = "192.0.2.15"
	path := filepath.Join(t.TempDir(), "server.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pair := func(serial int64) *tls.Certificate {
		der := testSubscriptionLeaf(t, &key.PublicKey, key, ip, time.Now().Add(time.Hour), serial)
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	}
	mgr := &subscriptionCertificates{host: ip, store: st}
	if err := mgr.replace(pair(1)); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{TLSConfig: &tls.Config{GetCertificate: mgr.getCertificate}, Handler: http.NotFoundHandler()}
	go func() { _ = server.ServeTLS(ln, "", "") }()
	defer ln.Close()
	defer server.Close()
	checkHandshake := func(serial int64) {
		t.Helper()
		roots := x509.NewCertPool()
		current, err := mgr.getCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		roots.AddCert(current.Leaf)
		// As on the public IP URL, the handshake must work without SNI.
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: ip, RootCAs: roots})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if got := conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); got != serial {
			t.Fatalf("served certificate serial %d, want %d", got, serial)
		}
	}
	checkHandshake(1)
	err = mgr.renew(t.Context(), func(context.Context) (*tls.Certificate, error) { return nil, errors.New("CA unavailable") })
	if err == nil {
		t.Fatal("renewal unexpectedly succeeded")
	}
	if ready, message := mgr.status(); !ready || !strings.Contains(message, "renewal") {
		t.Fatalf("renewal error did not preserve a ready endpoint: %t, %q", ready, message)
	}
	checkHandshake(1)
	if err := mgr.renew(t.Context(), func(context.Context) (*tls.Certificate, error) { return pair(2), nil }); err != nil {
		t.Fatal(err)
	}
	checkHandshake(2)
	expired := pair(3)
	expired.Leaf.NotAfter = time.Now().Add(-time.Second)
	if err := mgr.replace(expired); err == nil {
		t.Fatal("expired replacement accepted")
	}
	checkHandshake(2)
	_ = st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &subscriptionCertificates{host: ip, store: st}
	if err := restarted.restore(); err != nil {
		t.Fatal(err)
	}
	if restored, err := restarted.getCertificate(nil); err != nil || restored.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("last working certificate was not restored: %v", err)
	}
}
