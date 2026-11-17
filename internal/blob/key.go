package blob

import "strings"

func validateKey(key string, prefix bool) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") && !prefix {
		return ErrInvalidKey
	}
	trimmed := strings.TrimSuffix(key, "/")
	if trimmed == "" {
		return ErrInvalidKey
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrInvalidKey
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
				return ErrInvalidKey
			}
		}
	}
	return nil
}
