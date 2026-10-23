package admin

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"testing"
)

func TestAccountExecutionModeJSONCompatibility(t *testing.T) {
	var create CreateAccountRequest
	if err := json.Unmarshal([]byte(`{"name":"legacy","platform":"openai","type":"oauth","credentials":{}}`), &create); err != nil {
		t.Fatal(err)
	}
	mode, err := service.NormalizeAccountExecutionMode(create.ExecutionMode)
	if err != nil || mode != "normal" {
		t.Fatal("old API must default normal", mode, err)
	}
	var update UpdateAccountRequest
	if err := json.Unmarshal([]byte(`{"name":"renamed"}`), &update); err != nil {
		t.Fatal(err)
	}
	if update.ExecutionMode != nil {
		t.Fatal("omitted execution mode must remain nil")
	}
}

func TestAccountExecutionModeBackupRoundTrip(t *testing.T) {
	encoded, err := json.Marshal(DataAccount{Name: "vm", Platform: "openai", Type: "oauth", ExecutionMode: "docker_kernel"})
	if err != nil {
		t.Fatal(err)
	}
	var imported DataAccount
	if err = json.Unmarshal(encoded, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.ExecutionMode != "docker_kernel" {
		t.Fatal("backup lost execution mode")
	}
	var legacy DataAccount
	if err = json.Unmarshal([]byte(`{"name":"old"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	mode, err := service.NormalizeAccountExecutionMode(legacy.ExecutionMode)
	if err != nil || mode != "normal" {
		t.Fatal(mode, err)
	}
}
