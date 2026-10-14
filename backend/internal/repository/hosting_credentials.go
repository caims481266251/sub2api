package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const hostedTokenPrefix = "hosted-token:v1:"

// Use the existing persistent credential key for hosted OAuth tokens. Identity
// metadata remains queryable; access, refresh and ID tokens never persist in
// plaintext. Interceptors cover workers, admission and admin reads as well.
func configureHostedCredentials(client *ent.Client, enc service.OpenAICredentialEncryptor) {
	client.Account.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			m, ok := mutation.(*ent.AccountMutation)
			if !ok {
				return next.Mutate(ctx, mutation)
			}
			creds, changed := m.Credentials()
			if !changed || len(creds) == 0 {
				return next.Mutate(ctx, mutation)
			}
			_, owned := m.OwnerUserID()
			if !owned && !m.Op().Is(ent.OpCreate) {
				ids, err := m.IDs(ctx)
				if err != nil {
					return nil, err
				}
				owned, err = m.Client().Account.Query().Where(account.IDIn(ids...), account.OwnerUserIDNotNil()).Exist(ctx)
				if err != nil {
					return nil, err
				}
			}
			if owned {
				protected, err := encryptHostedTokens(enc, creds)
				if err != nil {
					return nil, err
				}
				m.SetCredentials(protected)
			}
			return next.Mutate(ctx, mutation)
		})
	})
	client.Account.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(ctx context.Context, query ent.Query) (ent.Value, error) {
			value, err := next.Query(ctx, query)
			if err != nil {
				return nil, err
			}
			decrypt := func(a *ent.Account) error {
				if a == nil {
					return nil
				}
				creds := make(map[string]any, len(a.Credentials))
				for k, v := range a.Credentials {
					if cipher, ok := v.(string); ok && strings.HasPrefix(cipher, hostedTokenPrefix) {
						plain, err := enc.Decrypt(strings.TrimPrefix(cipher, hostedTokenPrefix))
						if err != nil {
							return errors.New("hosted credential decryption unavailable")
						}
						creds[k] = plain
					} else {
						creds[k] = v
					}
				}
				a.Credentials = creds
				return nil
			}
			switch rows := value.(type) {
			case []*ent.Account:
				for _, a := range rows {
					if err := decrypt(a); err != nil {
						return nil, err
					}
				}
			case *ent.Account:
				if err := decrypt(rows); err != nil {
					return nil, err
				}
			}
			return value, nil
		})
	}))
}

// CAS must compare the original ciphertext, since encryption uses fresh nonces.
// Read and decrypt one snapshot, then let the caller's SQL equality guard reject
// any refresh/reauthorization that happens between this read and the mutation.
func accountCredentialsSnapshot(ctx context.Context, client *ent.Client, enc service.OpenAICredentialEncryptor, a *service.Account) ([]byte, error) {
	if a.OwnerUserID == nil {
		return json.Marshal(normalizeJSONMap(a.Credentials))
	}
	rows, err := client.QueryContext(ctx, "SELECT credentials FROM accounts WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL", a.ID, *a.OwnerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, service.ErrHostingAccountNotFound
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	for k, v := range stored {
		if cipher, ok := v.(string); ok && strings.HasPrefix(cipher, hostedTokenPrefix) {
			if enc == nil {
				return nil, errors.New("hosted credential decryption unavailable")
			}
			plain, err := enc.Decrypt(strings.TrimPrefix(cipher, hostedTokenPrefix))
			if err != nil {
				return nil, errors.New("hosted credential decryption unavailable")
			}
			stored[k] = plain
		}
	}
	actual, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	expected, err := json.Marshal(normalizeJSONMap(a.Credentials))
	if err != nil {
		return nil, err
	}
	if string(actual) != string(expected) {
		return nil, service.ErrUpstreamBillingProbeIdentityChanged
	}
	return raw, nil
}

func encryptHostedTokens(enc service.OpenAICredentialEncryptor, creds map[string]any) (map[string]any, error) {
	if enc == nil {
		return nil, errors.New("hosted credential encryption unavailable")
	}
	if _, err := enc.InitializeEncryption(); err != nil {
		return nil, errors.New("hosted credential encryption unavailable")
	}
	protected := make(map[string]any, len(creds))
	for k, v := range creds {
		if token, ok := v.(string); ok && token != "" && (k == "access_token" || k == "refresh_token" || k == "id_token") && !strings.HasPrefix(token, hostedTokenPrefix) {
			cipher, err := enc.Encrypt(token)
			if err != nil {
				return nil, errors.New("hosted credential encryption unavailable")
			}
			protected[k] = hostedTokenPrefix + cipher
		} else {
			protected[k] = v
		}
	}
	return protected, nil
}

func ProvideAccountRepository(client *ent.Client, db *sql.DB, cache service.SchedulerCache, enc service.OpenAICredentialEncryptor) service.AccountRepository {
	configureHostedCredentials(client, enc)
	r := NewAccountRepository(client, db, cache).(*accountRepository)
	r.hostingEncryptor = enc
	return r
}
