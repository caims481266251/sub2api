package tlsfingerprint

import "sync"

const maxHTTP2ProfileSnapshots = 256

// HTTP2ProfileCache reuses immutable snapshots across freshly decoded route
// profiles. Names are only lookup hints: every field is compared before reuse.
// FIFO eviction bounds retained route snapshots without adding writes on hits.
type HTTP2ProfileCache struct {
	mu      sync.RWMutex
	entries map[string]*HTTP2Profile
	order   [maxHTTP2ProfileSnapshots]string
	next    int
}

func (c *HTTP2ProfileCache) Compile(p *Profile) *HTTP2Profile {
	if p == nil {
		p = &Profile{}
	}
	if p.compiledHTTP2 != nil && sameProfile(p, &p.compiledHTTP2.source) {
		return p.compiledHTTP2
	}
	c.mu.RLock()
	entry := c.entries[p.Name]
	c.mu.RUnlock()
	if entry != nil && sameProfile(p, &entry.source) {
		return entry
	}
	compiled := CompileHTTP2(p)
	compiled.source = *CloneProfile(p)
	compiled.source.compiledHTTP2 = nil
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry = c.entries[p.Name]; entry != nil && sameProfile(p, &entry.source) {
		return entry
	}
	if c.entries == nil {
		c.entries = make(map[string]*HTTP2Profile)
	}
	if _, exists := c.entries[p.Name]; !exists {
		if len(c.entries) == maxHTTP2ProfileSnapshots {
			delete(c.entries, c.order[c.next])
		}
		c.order[c.next] = p.Name
		c.next = (c.next + 1) % maxHTTP2ProfileSnapshots
	}
	c.entries[p.Name] = compiled
	return compiled
}
