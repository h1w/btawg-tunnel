package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var bucketSubscriptions = []byte("subscriptions")

// SubscriptionToken returns the current token, or an empty string if not issued.
func (s *Store) SubscriptionToken(id string) (string, error) {
	var token string
	err := s.db.View(func(tx *bolt.Tx) error {
		if secret := tx.Bucket(bucketSubscriptions).Get([]byte(id)); secret != nil {
			token = id + "." + base64.RawURLEncoding.EncodeToString(secret)
		}
		return nil
	})
	return token, err
}

// RotateSubscription replaces only the subscription credential of an active client.
func (s *Store) RotateSubscription(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if _, err := activeSubscriptionClient(tx, []byte(id)); err != nil {
			return err
		}
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return err
		}
		return tx.Bucket(bucketSubscriptions).Put([]byte(id), secret[:])
	})
}

// SubscriptionClient authenticates a URL token and reads its enrolled client.
func (s *Store) SubscriptionClient(token string) (ClientRecord, error) {
	id, suffix, ok := strings.Cut(token, ".")
	var secret [32]byte
	if !ok || id == "" || len(suffix) != base64.RawURLEncoding.EncodedLen(len(secret)) {
		return ClientRecord{}, ErrNotFound
	}
	if n, err := base64.RawURLEncoding.Strict().Decode(secret[:], []byte(suffix)); err != nil || n != len(secret) {
		return ClientRecord{}, ErrNotFound
	}
	var rec ClientRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		key := []byte(id)
		if subtle.ConstantTimeCompare(secret[:], tx.Bucket(bucketSubscriptions).Get(key)) != 1 {
			return ErrNotFound
		}
		var err error
		rec, err = activeSubscriptionClient(tx, key)
		return err
	})
	return rec, err
}

func activeSubscriptionClient(tx *bolt.Tx, key []byte) (ClientRecord, error) {
	var rec ClientRecord
	raw := tx.Bucket(bucketClients).Get(key)
	if raw == nil {
		return rec, ErrNotFound
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, err
	}
	if rec.Revoked {
		return ClientRecord{}, ErrNotFound
	}
	NormalizeClientRecord(&rec)
	return rec, nil
}
