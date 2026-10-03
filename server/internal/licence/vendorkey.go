package licence

import "encoding/base64"

// The vendor's public key, 32 bytes base64, pasted from the output of
// `dialler-licence keygen`. The private half never enters the repository.
// The same bytes are embedded in the app's verifier; rotating the key
// strands every licence issued under the old one, so it is a decision, not
// a routine.
//
// Empty until the key is generated: a build with no key verifies nothing,
// which is the safe direction (0 seats), never the other.
const vendorPublicKeyBase64 = ""

func init() {
	PublicKey = mustKey(vendorPublicKeyBase64)
}

// mustKey decodes the embedded key, or returns nil for an empty or
// unusable literal.
func mustKey(b64 string) []byte {
	if b64 == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(key) != 32 {
		return nil
	}
	return key
}
