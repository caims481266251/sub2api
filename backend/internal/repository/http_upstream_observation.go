package repository

import (
	"net/http"
	"net/url"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

func upstreamTransportInfo(kind string, proxy *url.URL, cacheKey, poolKey, fingerprint string) requesttiming.TransportInfo {
	mode := "direct"
	if proxy != nil {
		mode = proxy.Scheme
	}
	return requesttiming.TransportInfo{
		Kind: kind, ProxyMode: mode,
		PoolID: safeUpstreamPoolID(cacheKey + "\x00" + poolKey), FingerprintID: fingerprint,
	}
}

func observeUpstreamTransport(req *http.Request, entry *upstreamClientEntry) *http.Request {
	if requesttiming.From(req.Context()) == nil {
		return req
	}
	info := entry.observation
	info.IdentityID = requesttiming.IdentityDigest(req.Header)
	return req.WithContext(requesttiming.WithTransport(req.Context(), info))
}
