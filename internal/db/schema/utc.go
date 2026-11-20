package schema

import "time"

// UTCNow returns the current timestamp in UTC so SQLite TEXT timestamps and
// PostgreSQL TIMESTAMPTZ values stay comparable regardless of host timezone.
func UTCNow() time.Time {
	return time.Now().UTC()
}
