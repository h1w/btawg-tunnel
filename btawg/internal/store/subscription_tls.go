package store

import (
	"bytes"

	bolt "go.etcd.io/bbolt"
)

var subscriptionAccountKey = []byte("subscription-acme-account")

// LoadSubscriptionAccount reads the ACME account's PKCS#8 private key.
func (s *Store) LoadSubscriptionAccount() ([]byte, error) {
	return s.loadSubscriptionTLS(subscriptionAccountKey)
}

// SaveSubscriptionAccount persists the ACME account key before registration.
func (s *Store) SaveSubscriptionAccount(key []byte) error {
	return s.saveSubscriptionTLS(subscriptionAccountKey, key)
}

// LoadSubscriptionCertificate reads a cached certificate chain and key in PEM.
func (s *Store) LoadSubscriptionCertificate(host string) ([]byte, error) {
	return s.loadSubscriptionTLS([]byte("subscription-acme-cert:" + host))
}

// SaveSubscriptionCertificate replaces the certificate and key together.
func (s *Store) SaveSubscriptionCertificate(host string, pair []byte) error {
	return s.saveSubscriptionTLS([]byte("subscription-acme-cert:"+host), pair)
}

func (s *Store) loadSubscriptionTLS(key []byte) ([]byte, error) {
	var value []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		value = bytes.Clone(tx.Bucket(bucketServer).Get(key))
		return nil
	})
	return value, err
}

func (s *Store) saveSubscriptionTLS(key, value []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketServer).Put(key, value)
	})
}
