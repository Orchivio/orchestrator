package admin

import "time"

// SetTimeouts shortens the adapter call deadlines for tests.
func SetTimeouts(s *Service, refresh, check time.Duration) {
	s.refreshTimeout, s.checkTimeout = refresh, check
}
