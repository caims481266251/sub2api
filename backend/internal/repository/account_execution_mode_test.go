package repository

import (
	"context"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"regexp"
	"strings"
	"testing"
)

func TestSchedulerExecutionModeCacheBoundary(t *testing.T) {
	old, err := decodeCachedAccount(`{"ID":1,"Platform":"openai"}`)
	if err != nil || old != nil {
		t.Fatal("legacy cache must miss", old, err)
	}
	for _, mode := range []string{"", service.AccountExecutionModeVMKernel} {
		full, meta, err := marshalSchedulerCacheAccount(service.Account{ID: 1, ExecutionMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		for _, payload := range [][]byte{full, meta} {
			got, err := decodeCachedAccount(payload)
			want := mode
			if want == "" {
				want = service.AccountExecutionModeNormal
			}
			if err != nil || got == nil || got.ExecutionMode != want {
				t.Fatalf("mode %q: got %+v err=%v", mode, got, err)
			}
		}
	}
}

func TestAccountExecutionModeEntityMapping(t *testing.T) {
	for _, mode := range []string{service.AccountExecutionModeNormal, service.AccountExecutionModeVMKernel} {
		got := accountEntityToService(&dbent.Account{ID: 7, ExecutionMode: mode})
		if got.ExecutionMode != mode {
			t.Fatalf("stored mode %q mapped as %q", mode, got.ExecutionMode)
		}
	}
}

func TestAccountExecutionModeMaintenanceUpdateDoesNotOverwriteStoredMode(t *testing.T) {
	for _, mode := range []string{service.AccountExecutionModeNormal, service.AccountExecutionModeVMKernel} {
		t.Run(mode, func(t *testing.T) {
			matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
				if expected == "immutable mode update" {
					if !strings.HasPrefix(actual, `UPDATE "accounts" SET `) || strings.Contains(actual, `"execution_mode" =`) {
						return fmt.Errorf("maintenance update changed immutable mode: %s", actual)
					}
					return nil
				}
				return sqlmock.QueryMatcherRegexp.Match(expected, actual)
			})
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
			if err != nil {
				t.Fatal(err)
			}
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"identity", "ollama_group", "ollama_proxy", "enabled", "rate_sync", "snapshot", "session", "auto", "ollama_snapshot", "opencode_group", "opencode_auto", "opencode_snapshot", "current_extra"}).AddRow(true, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, []byte(`{}`)))
			mock.ExpectExec("immutable mode update").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`SELECT .* FROM "accounts" WHERE "id" = \$1`).WithArgs(int64(7)).WillReturnRows(updatedAccountRows(7, `{}`, mode))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()
			partial := &service.Account{ID: 7, Name: "renamed", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Concurrency: 1, Priority: 1, Credentials: map[string]any{}, Extra: map[string]any{}}
			if err := repo.Update(context.Background(), partial); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
