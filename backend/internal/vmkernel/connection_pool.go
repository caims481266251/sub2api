package vmkernel

import (
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"time"
)

// These bounds retain idle resources only; they never cap active requests.
const kernelPoolLimit = 256
const kernelIdleConnections = 32
const kernelIdleTimeout = 90 * time.Second

type kernelPoolIdentity struct {
	accountID                         int64
	instance, boot, credential, proxy string
	generation                        uint64
	token                             [32]byte
	headerTimeout                     time.Duration
}

type kernelConnectionPool struct {
	identity  kernelPoolIdentity
	transport *http.Transport
	client    *http.Client
	lastUsed  time.Time
	retired   bool
}

// Each socket owns one current pool, fenced by account and execution identity.
// Retiring a pool closes idle connections without interrupting active streams.
func (c *Client) acquireConnectionPool(binding Binding) (*http.Client, func(), error) {
	c.poolMu.Lock()
	defer c.poolMu.Unlock()
	if c.closed {
		return nil, nil, &AttemptError{Code: "kernel_client_closed", SafeToRetry: true}
	}
	identity := kernelPoolIdentity{accountID: binding.AccountID, instance: binding.InstanceID,
		boot: binding.GuestBootID, credential: binding.CredentialRevision, proxy: binding.ProxyRevision,
		generation: binding.Generation, token: sha256.Sum256([]byte(binding.Token)), headerTimeout: c.ResponseHeaderTimeout}
	if c.pools == nil {
		c.pools = make(map[string]*kernelConnectionPool)
	}
	now := time.Now()
	pool := c.pools[binding.Socket]
	if pool != nil && (pool.identity != identity || now.Sub(pool.lastUsed) >= kernelIdleTimeout) {
		c.retirePool(binding.Socket, pool)
		pool = nil
	}
	if pool == nil {
		// Bound retained pool metadata even when many accounts are deleted. An
		// evicted in-flight transport is closed again when its request finishes.
		if len(c.pools) >= kernelPoolLimit {
			var oldest *kernelConnectionPool
			var socket string
			for key, candidate := range c.pools {
				if oldest == nil || candidate.lastUsed.Before(oldest.lastUsed) {
					oldest, socket = candidate, key
				}
			}
			c.retirePool(socket, oldest)
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", binding.Socket)
			},
			ResponseHeaderTimeout: c.ResponseHeaderTimeout, MaxResponseHeaderBytes: 64 << 10,
			MaxIdleConns: kernelIdleConnections, MaxIdleConnsPerHost: kernelIdleConnections,
			IdleConnTimeout: kernelIdleTimeout,
			// MaxConnsPerHost remains zero: original account admission owns concurrency.
		}
		pool = &kernelConnectionPool{identity: identity, transport: transport,
			client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		c.pools[binding.Socket] = pool
	}
	pool.lastUsed = now
	return pool.client, func() {
		c.poolMu.Lock()
		defer c.poolMu.Unlock()
		if pool.retired {
			pool.transport.CloseIdleConnections()
		} else {
			pool.lastUsed = time.Now()
		}
	}, nil
}

// Caller holds poolMu. Active requests retain their own transport until release.
func (c *Client) retirePool(socket string, pool *kernelConnectionPool) {
	pool.retired = true
	pool.transport.CloseIdleConnections()
	delete(c.pools, socket)
}

// Close releases local connections, not the account's container or kernel.
func (c *Client) Close() {
	c.poolMu.Lock()
	defer c.poolMu.Unlock()
	c.closed = true
	for socket, pool := range c.pools {
		c.retirePool(socket, pool)
	}
}
