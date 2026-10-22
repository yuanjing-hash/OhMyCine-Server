package models

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// QuarkObservationAccountKey groups only the namespace-qualified TV reference
// persisted after TV identity verification. Unknown/legacy identities retain a
// connection-private key; Agent/Web IDs are never inferred to be equivalent.
func QuarkObservationAccountKey(connectionID uint, accountReference string) string {
	identity := fmt.Sprintf("quark:%d:%s", connectionID, accountReference)
	if strings.HasPrefix(accountReference, "quark:") {
		encoded := strings.TrimPrefix(accountReference, "quark:")
		decoded, err := hex.DecodeString(encoded)
		if err == nil && len(decoded) == 16 && encoded == strings.ToLower(encoded) {
			identity = "quark-tv-observation\x00" + accountReference
		}
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}
