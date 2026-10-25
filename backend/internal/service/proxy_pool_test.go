package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProxyPoolParsingCanonicalizationAndRedaction(t *testing.T) {
	entries, duplicates, issues := parseProxyPool("EXAMPLE.COM:8080:user:p:a@ss\r\nhttp://user:p%3Aa%40ss@example.com:8080/\n\n[::1]:1080:user:password\nsocks5://other:secret@example.net:1080\n")
	require.Empty(t, issues)
	require.Equal(t, 1, duplicates)
	require.Len(t, entries, 3)
	require.Equal(t, "http://user:p%3Aa%40ss@example.com:8080", entries[0].URL)
	require.Equal(t, "::1", entries[1].Host)
	require.Equal(t, "socks5h", entries[2].Protocol)
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	for _, secret := range []string{"password", "p%3Aa%40ss", "other", "secret", entries[0].Identity} {
		require.NotContains(t, string(data), secret)
	}
	// Authentication is part of entry identity, so provider session IDs remain distinct.
	a, err := parseProxyPoolLine("example.com:80:user-session-a:secret")
	require.NoError(t, err)
	b, err := parseProxyPoolLine("example.com:80:user-session-b:secret")
	require.NoError(t, err)
	require.NotEqual(t, a.Identity, b.Identity)
	urlPassword, err := parseProxyPoolLine("example.com:80:user:http://secret/path")
	require.NoError(t, err)
	encodedPassword, err := parseProxyPoolLine("http://user:http%3A%2F%2Fsecret%2Fpath@example.com:80")
	require.NoError(t, err)
	require.Equal(t, encodedPassword.Identity, urlPassword.Identity)
	emptyPassword, err := parseProxyPoolLine("http://user@example.com:80")
	require.NoError(t, err)
	explicitEmpty, err := parseProxyPoolLine("example.com:80:user:")
	require.NoError(t, err)
	require.Equal(t, emptyPassword.Identity, explicitEmpty.Identity)
}
func TestProxyPoolRejectsInvalidBatchesWithoutEchoingSecrets(t *testing.T) {
	for _, line := range []string{"http://user:do-not-echo@%zz:80", "ftp://user:do-not-echo@example.com:21", "example.com:65536:user:do-not-echo", "http://user:do-not-echo@example.com:80/path", "http://user:do-not-echo@example.com:80?x=1", "http://user:do-not-echo@example.com:80/#bad", "http://user:do-not-echo@example.com", "http://user%0a:do-not-echo@example.com:80", "http://:do-not-echo@example.com:80", "http://user:do-not-echo@-bad.com:80"} {
		_, _, issues := parseProxyPool("example.com:80\n" + line)
		require.Equal(t, []ProxyPoolIssue{{Line: 2, Reason: "invalid_proxy"}}, issues)
		b, err := json.Marshal(issues)
		require.NoError(t, err)
		require.NotContains(t, string(b), "do-not-echo")
	}
	_, _, issues := parseProxyPool(strings.Repeat("example.com:80\n", ProxyPoolMaxEntries+1))
	require.Equal(t, "input_limit", issues[0].Reason)
	_, _, issues = parseProxyPool("\n \n")
	require.Equal(t, "empty_input", issues[0].Reason)
}

type poolServiceRepo struct {
	ProxyPoolRepository
	state   ProxyPoolState
	applied bool
}

func (r *poolServiceRepo) Load(context.Context) (ProxyPoolState, error) { return r.state, nil }
func (r *poolServiceRepo) Apply(context.Context, int64, string, []ProxyPoolEntry) (ProxyPoolState, error) {
	r.applied = true
	return r.state, nil
}

type poolServiceEncryption struct {
	OpenAICredentialEncryptor
	configured bool
}

func (e poolServiceEncryption) EncryptionStatus() (CredentialEncryptionStatus, error) {
	return CredentialEncryptionStatus{Configured: e.configured}, nil
}
func TestProxyPoolPreviewAndImportAreAtomicAndRevisionChecked(t *testing.T) {
	a, err := parseProxyPoolLine("one.example:80")
	require.NoError(t, err)
	b, err := parseProxyPoolLine("two.example:80")
	require.NoError(t, err)
	r := &poolServiceRepo{state: ProxyPoolState{Revision: 3, Entries: []ProxyPoolEntry{a, b}}}
	s := NewProxyPoolService(r, poolServiceEncryption{configured: true})
	in := ProxyPoolImport{Revision: 3, Mode: "replace", Text: "one.example:80\nnew.example:80\nnew.example:80"}
	p, err := s.Preview(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 1, p.AddedCount)
	require.Equal(t, 1, p.RetainedCount)
	require.Equal(t, 1, p.RemovedCount)
	require.Equal(t, 1, p.DuplicateCount)
	require.True(t, p.CanApply)
	require.False(t, r.applied)
	in.Text += "\nbad-proxy"
	p, err = s.Preview(t.Context(), in)
	require.NoError(t, err)
	require.False(t, p.CanApply)
	_, err = s.Import(t.Context(), in)
	require.ErrorIs(t, err, ErrProxyPoolInvalid)
	require.False(t, r.applied)
	in.Revision = 2
	_, err = s.Import(t.Context(), in)
	require.ErrorIs(t, err, ErrProxyPoolConflict)
	require.False(t, r.applied)
	in.Revision = 3
	in.Text = "one.example:80"
	s.encryptor = poolServiceEncryption{}
	_, err = s.Import(t.Context(), in)
	require.ErrorIs(t, err, ErrProxyPoolEncryption)
	require.False(t, r.applied)
}
