package syncer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"seatime/internal/client/config"
	"seatime/pkg/api"
	"seatime/pkg/chain"
)

// NewKey generates a device keypair (base64 private, public).
func NewKey() (priv, pub string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(sk), base64.StdEncoding.EncodeToString(pk), nil
}

func privKey(a config.Auth) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(a.PrivateKey)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("device key missing or corrupt; sign this computer in again")
	}
	return ed25519.PrivateKey(b), nil
}

// CheckpointMessage is exactly what pkg/api says is signed for a checkpoint.
func CheckpointMessage(deviceID string, seq int64, hash string) []byte {
	return []byte(fmt.Sprintf("seatime-checkpoint\n%s\n%d\n%s", deviceID, seq, hash))
}

func SignCheckpoint(a config.Auth, cp chain.Checkpoint) (string, error) {
	k, err := privKey(a)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k, CheckpointMessage(a.DeviceID, cp.Seq, cp.Hash))), nil
}

func VerifyCheckpoint(pub, sig, deviceID string, cp chain.Checkpoint) bool {
	pk, err1 := base64.StdEncoding.DecodeString(pub)
	sg, err2 := base64.StdEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || len(pk) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pk, CheckpointMessage(deviceID, cp.Seq, cp.Hash), sg)
}

// BundleDigest is sha256 of encoding/json of the bundle with Sig cleared.
func BundleDigest(b api.Bundle) []byte {
	b.Sig = ""
	j, _ := json.Marshal(b)
	d := sha256.Sum256(j)
	return d[:]
}

func SignBundle(a config.Auth, b *api.Bundle) error {
	k, err := privKey(a)
	if err != nil {
		return err
	}
	b.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(k, BundleDigest(*b)))
	return nil
}

func VerifyBundle(pub string, b api.Bundle) bool {
	pk, err1 := base64.StdEncoding.DecodeString(pub)
	sg, err2 := base64.StdEncoding.DecodeString(b.Sig)
	if err1 != nil || err2 != nil || len(pk) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pk, BundleDigest(b), sg)
}

// signText signs a plain message with the device key (base64).
func signText(a config.Auth, msg string) (string, error) {
	k, err := privKey(a)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k, []byte(msg))), nil
}
