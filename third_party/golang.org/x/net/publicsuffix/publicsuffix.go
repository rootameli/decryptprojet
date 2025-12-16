package publicsuffix

import (
	"errors"
	"strings"
)

// EffectiveTLDPlusOne returns the effective top level domain plus one label.
// This is a minimal offline replacement that preserves deterministic behaviour
// without depending on the upstream publicsuffix list.
func EffectiveTLDPlusOne(domain string) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", errors.New("publicsuffix: empty domain")
	}
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return "", errors.New("publicsuffix: domain missing TLD")
	}
	return strings.ToLower(strings.Join(parts[len(parts)-2:], ".")), nil
}
