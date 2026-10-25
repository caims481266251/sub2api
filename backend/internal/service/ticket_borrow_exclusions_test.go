package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBorrowSelectionExclusionValidation(t *testing.T) {
	v := BorrowSelection{Revision: 1, DefaultModel: "test-model", AccountIDs: []int64{1, 2, 3}, ExcludedAccountIDs: []int64{2, 2, 1}}
	require.NoError(t, normalizeBorrowSelection(&v))
	require.Equal(t, []int64{1, 2}, v.ExcludedAccountIDs)
	require.Equal(t, []int64{3}, v.AccountIDs)
	for _, ids := range [][]int64{{0}, {-1}, make([]int64, 1001)} {
		v.ExcludedAccountIDs = ids
		require.EqualError(t, normalizeBorrowSelection(&v), "borrow_selection_invalid")
	}
}

func TestBorrowSelectionExclusionOmission(t *testing.T) {
	for _, payload := range []string{
		`{"revision":1,"default_model":"test-model"}`,
		`{"revision":1,"default_model":"test-model","excluded_account_ids":null}`,
		`{"revision":1,"default_model":"test-model","excluded_account_ids":[]}`,
	} {
		var v BorrowSelection
		require.NoError(t, json.Unmarshal([]byte(payload), &v))
		omitted := v.ExcludedAccountIDs == nil
		require.NoError(t, normalizeBorrowSelection(&v))
		require.Equal(t, omitted, v.ExcludedAccountIDs == nil)
	}
}
