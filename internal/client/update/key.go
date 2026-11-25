package update

// PublicKey is the release signing key (ed25519, base64), compiled into every
// seatime binary. This is the one place to set it:
//
//	go run ./cmd/seatime-release keygen ~/seatime-release.key
//
// prints the public key; paste it here and rebuild. Until a real key is set
// here, updates refuse to run (fail closed). The private key stays with
// whoever cuts releases; seatimed never needs it.
const PublicKey = "o7MA7wKJWe+YZw5sQnauWZDp3JirAJS7b+EVvTIewr4="

// PublicKeys are every release key a client accepts: PublicKey and the
// next one, during a rotation. To rotate, add the new key here and
// ship a release signed with the old one; once clients have it, sign with
// the new key and make it PublicKey. A leaked key is dropped the same way.
// A manifest is accepted if any of them signed it.
var PublicKeys = []string{PublicKey}
