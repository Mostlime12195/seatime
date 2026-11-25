package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

func decodePub(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

func verifyEd(pub ed25519.PublicKey, msg, sig []byte) bool {
	return len(sig) == ed25519.SignatureSize && ed25519.Verify(pub, msg, sig)
}
