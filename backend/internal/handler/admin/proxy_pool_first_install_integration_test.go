//go:build integration

package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Exercise the actual embedded SQL and HTTP handlers, without fixture DDL,
// seeded proxy-pool rows, a pre-existing key, or manual database preparation.
func TestProxyPoolFirstInstall(t *testing.T) {
	previousMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previousMode) })
	db := newProxyPoolFirstInstallDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	keyPath := filepath.Join(dir, "secrets", "credential-operations.key")

	var tables int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables))
	require.Zero(t, tables, "first installation must start from an empty database")
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	assertAllProxyPoolFirstInstallMigrations(t, db)

	// Config loading supplies an ephemeral fallback key but leaves Configured
	// false when the administrator has not set TOTP_ENCRYPTION_KEY.
	cfg := &config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("ab", 32)}}
	newHandler := func() (*ProxyHandler, service.OpenAICredentialEncryptor) {
		fallback, err := repository.NewAESEncryptor(cfg)
		require.NoError(t, err)
		enc := repository.NewOpenAICredentialEncryptor(cfg, fallback, db)
		return ProvideProxyHandler(nil, repository.NewProxyPoolRepository(db, enc), enc), enc
	}
	h, _ := newHandler()
	request := func(method, path, body string, handle gin.HandlerFunc) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		handle(c)
		return w
	}
	stateFrom := func(w *httptest.ResponseRecorder) service.ProxyPoolState {
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var response struct {
			Code int                    `json:"code"`
			Data service.ProxyPoolState `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		require.Zero(t, response.Code)
		return response.Data
	}

	state := stateFrom(request(http.MethodGet, "/api/v1/admin/proxies/pool", "", h.PoolState))
	require.EqualValues(t, 1, state.Revision)
	require.NotNil(t, state.Entries)
	require.Empty(t, state.Entries)
	require.False(t, state.EncryptionConfigured)
	_, err := os.Stat(keyPath)
	require.True(t, os.IsNotExist(err), "opening the page must not create a key")

	state = stateFrom(request(http.MethodPost, "/api/v1/admin/proxies/pool/encryption", "", h.PoolEncryption))
	require.True(t, state.EncryptionConfigured)
	key, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	const input = `{"revision":1,"mode":"append","text":"example.test:8080:fixture-user:fixture-password"}`
	w := request(http.MethodPost, "/api/v1/admin/proxies/pool/preview", input, h.PoolImport)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var preview struct {
		Data service.ProxyPoolPreview `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &preview))
	require.True(t, preview.Data.CanApply)
	require.Equal(t, 1, preview.Data.AddedCount)
	require.NotContains(t, w.Body.String(), "fixture-password")
	w = request(http.MethodPut, "/api/v1/admin/proxies/pool", input, h.PoolImport)
	state = stateFrom(w)
	require.EqualValues(t, 2, state.Revision)
	require.Len(t, state.Entries, 1)
	require.NotContains(t, w.Body.String(), "fixture-password")

	// A restart reruns schema initialization without resetting rows or the key.
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	assertAllProxyPoolFirstInstallMigrations(t, db)
	cfg.Totp.EncryptionKey = strings.Repeat("cd", 32)
	restarted, enc := newHandler()
	after := stateFrom(request(http.MethodGet, "/api/v1/admin/proxies/pool", "", restarted.PoolState))
	require.Equal(t, state, after)
	stateFrom(request(http.MethodPost, "/api/v1/admin/proxies/pool/encryption", "", restarted.PoolEncryption))
	afterKey, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	require.Equal(t, key, afterKey)
	var ciphertext string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT credential_ciphertext FROM proxy_pool_entries WHERE id=$1`, after.Entries[0].ID).Scan(&ciphertext))
	require.NotContains(t, ciphertext, "fixture-password")
	plaintext, err := enc.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, "http://fixture-user:fixture-password@example.test:8080", plaintext)
}

func assertAllProxyPoolFirstInstallMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT filename,checksum FROM schema_migrations`)
	require.NoError(t, err)
	defer rows.Close()
	applied := map[string]string{}
	for rows.Next() {
		var filename, checksum string
		require.NoError(t, rows.Scan(&filename, &checksum))
		applied[filename] = checksum
	}
	require.NoError(t, rows.Err())
	files, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	count := 0
	for _, filename := range files {
		content, err := migrations.FS.ReadFile(filename)
		require.NoError(t, err)
		trimmed := strings.TrimSpace(string(content))
		if trimmed == "" {
			continue
		}
		count++
		checksum := sha256.Sum256([]byte(trimmed))
		require.Equal(t, hex.EncodeToString(checksum[:]), applied[filename], filename)
	}
	require.Len(t, applied, count, "every embedded migration must finish on first startup")
}

func newProxyPoolFirstInstallDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := t.Context()
	// Optional local runner for machines without Docker. Only use a disposable
	// PostgreSQL server; a dedicated database is created and dropped below.
	dsn := os.Getenv("SUB2API_FRESH_INSTALL_TEST_POSTGRES_URL")
	if dsn == "" {
		container, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23",
			tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
		dsn, err = container.ConnectionString(ctx, "sslmode=disable")
		require.NoError(t, err)
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, u.Scheme)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	name := fmt.Sprintf("proxy_pool_first_install_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
	})
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}
