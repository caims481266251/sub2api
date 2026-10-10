package repository

import "github.com/Wei-Shaw/sub2api/internal/service"

// Probe traffic has its own connection cache and client-count budget. Separate
// keys alone would still let active probes exhaust the business cache limit.
func (s *httpUpstreamService) borrowProbePool() *httpUpstreamService {
	s.probePoolOnce.Do(func() {
		s.probePool = &httpUpstreamService{cfg: s.cfg, clients: make(map[string]*upstreamClientEntry), probeOnly: true}
	})
	return s.probePool
}

// Installed during construction, before the server accepts requests.
func (s *httpUpstreamService) SetTicketBorrowService(b *service.TicketBorrowService) {
	s.borrowRoutes = b
}
func (s *astraRoutingUpstream) SetTicketBorrowService(b *service.TicketBorrowService) {
	s.borrowRoutes = b
	if raw, ok := s.delegate.(interface {
		SetTicketBorrowService(*service.TicketBorrowService)
	}); ok {
		raw.SetTicketBorrowService(b)
	}
}
