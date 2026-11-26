package alert

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/eimsound/lazy-observe/internal/alert/matcher"
)

// Fingerprint returns a stable SHA-256 identity for an alert label set,
// independent of map iteration order.
func Fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ValidateMatchers verifies that a matcher slice is non-empty and every entry
// has a non-empty label name, supported operator (`=` or `=~`), and valid regex.
func ValidateMatchers(matchers []Matcher) error {
	return matcher.ValidateSet(matchers)
}
