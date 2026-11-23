package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestSubscriptionDefaultsToHubIPv4(t *testing.T) {
	const publicIP = "192.0.2.15"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{IPAddresses: []net.IP{net.ParseIP(publicIP)}, DNSNames: []string{"profiles.example.org"}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writePEM := func(name, kind string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	settings := subscriptionSettings{
		SubscriptionCert: writePEM("cert.pem", "CERTIFICATE", der),
		SubscriptionKey:  writePEM("key.pem", "PRIVATE KEY", key),
	}
	tlsConfig, err := settings.loadTLS()
	if err != nil {
		t.Fatalf("PEM files without an explicit host must enable subscriptions: %v", err)
	}
	if host, err := settings.publicHost(publicIP, tlsConfig); err != nil || host != publicIP {
		t.Fatalf("resolved host = %q, %v; want the active hub IP", host, err)
	}
	for _, ip := range []string{"", "10.0.0.15", "192.0.2.16"} {
		if _, err := settings.publicHost(ip, tlsConfig); err == nil {
			t.Errorf("accepted missing, private or uncertified address %q", ip)
		}
	}
	settings.SubscriptionHost = "profiles.example.org"
	tlsConfig, err = settings.loadTLS()
	if err != nil {
		t.Fatal(err)
	}
	if host, err := settings.publicHost(publicIP, tlsConfig); err != nil || host != "profiles.example.org" {
		t.Fatalf("explicit host = %q, %v", host, err)
	}
}
