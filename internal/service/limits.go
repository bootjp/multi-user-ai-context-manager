package service

import "time"

type bucket struct {
	tokens float64
	at     time.Time
}

// Per-process token buckets. Apply an ingress limit as well when running replicas.
func (s *Server) allow(key string, now time.Time) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rates == nil {
		s.rates = map[string]bucket{}
	}
	if len(s.rates) >= 10000 {
		for k, b := range s.rates {
			if now.Sub(b.at) > time.Minute {
				delete(s.rates, k)
			}
		}
		if len(s.rates) >= 10000 {
			if _, ok := s.rates[key]; !ok {
				return false
			}
		}
	}
	b, ok := s.rates[key]
	if !ok {
		b = bucket{40, now}
	}
	b.tokens = min(40, b.tokens+now.Sub(b.at).Seconds()*20)
	b.at = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	s.rates[key] = b
	return allowed
}
