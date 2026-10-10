//go:build integration

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowObservationRoundTripAndConnectedRequest(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,stage,reason) VALUES($1,'target','historical_result')`, id)
	require.NoError(t, err)
	age := int64(0)
	expires := time.Now().UTC().Add(time.Minute)
	details := &service.BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: q.BatchID, SourceModel: "gpt-6-luna", CookieAgeAtProbeMS: &age, ExpiresAt: &expires, Gateway: "chat.gateway.unified-1.api.openai.com", ProbeVerdict: "degraded"}
	require.NoError(t, r.Event(ctx, service.BorrowEvent{TargetID: id, Stage: "target", Reason: "target_ticket_changed", Details: details}))
	events, err := r.Events(ctx, 0, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Nil(t, events[1].Details)
	require.Equal(t, details.CandidateID, events[0].Details.CandidateID)
	require.NotNil(t, events[0].Details.CookieAgeAtProbeMS)
	require.Zero(t, *events[0].Details.CookieAgeAtProbeMS)
	require.WithinDuration(t, expires, *events[0].Details.ExpiresAt, time.Microsecond)

	q.ID, q.CandidateID = uuid.NewString(), details.CandidateID
	q.Environment = &service.BorrowEnvironment{IdentityHash: "before-connection", BodyHash: "body-hash"}
	require.NoError(t, r.BeginTargetRequest(ctx, q))
	q.Environment = &service.BorrowEnvironment{IdentityHash: "connected-identity", BodyHash: "body-hash"}
	require.NoError(t, r.MarkTargetRequestSent(ctx, q))
	q.Environment = &service.BorrowEnvironment{IdentityHash: "later-connection"}
	require.NoError(t, r.MarkTargetRequestSent(ctx, q))
	var raw []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT details FROM ticket_borrow_requests WHERE id=$1`, q.ID).Scan(&raw))
	var observation struct {
		CandidateID string                    `json:"candidate_id"`
		Environment service.BorrowEnvironment `json:"environment"`
	}
	require.NoError(t, json.Unmarshal(raw, &observation))
	require.Equal(t, details.CandidateID, observation.CandidateID)
	require.Equal(t, "connected-identity", observation.Environment.IdentityHash)
	require.Equal(t, "body-hash", observation.Environment.BodyHash)
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "eof"))

	q = service.BorrowRequest{ID: uuid.NewString(), TargetID: id, Kind: "business", RouteID: uuid.NewString()}
	require.NoError(t, r.BeginTargetRequest(ctx, q))
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM ticket_borrow_requests WHERE id=$1`, q.ID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "eof"))
}
