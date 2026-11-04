package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func runClientKeyUsage(t *testing.T, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
		ginCtx.Request = httptest.NewRequest(method, "/v8/management/observability/usage/client-keys", reader)
		ginCtx.Request.Header.Set("Content-Type", "application/json")
	} else {
		ginCtx.Request = httptest.NewRequest(method, "/v8/management/observability/usage/client-keys", nil)
	}
	h := &Handler{}
	if method == http.MethodGet {
		h.GetClientKeyUsage(ginCtx)
	} else {
		h.DeleteClientKeyUsage(ginCtx)
	}
	return rec
}

func TestClientKeyUsageGetAndReset(t *testing.T) {
	store := clientusage.Default()
	store.Reset("")
	t.Cleanup(func() { store.Reset("") })
	store.Add(clientusage.Event{APIKey: "key-a", Model: "gpt-5", Detail: coreusage.Detail{TotalTokens: 42}})
	store.Add(clientusage.Event{APIKey: "key-b", Model: "gpt-5", Detail: coreusage.Detail{TotalTokens: 1}})

	rec := runClientKeyUsage(t, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Enabled *bool                           `json:"enabled"`
		Keys    map[string]clientusage.KeyUsage `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Enabled == nil {
		t.Fatal("response has no enabled flag")
	}
	if payload.Keys["key-a"].TotalTokens != 42 {
		t.Fatalf("key-a = %+v", payload.Keys["key-a"])
	}

	if rec = runClientKeyUsage(t, http.MethodDelete, `{"key":"missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing status = %d", rec.Code)
	}
	if rec = runClientKeyUsage(t, http.MethodDelete, `{"key":"key-a"}`); rec.Code != http.StatusOK {
		t.Fatalf("DELETE key-a status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Snapshot().Keys["key-a"]; ok {
		t.Fatal("key-a still present after reset")
	}
	if rec = runClientKeyUsage(t, http.MethodDelete, `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE bad body status = %d", rec.Code)
	}
	if rec = runClientKeyUsage(t, http.MethodDelete, ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE all status = %d", rec.Code)
	}
	if len(store.Snapshot().Keys) != 0 {
		t.Fatal("keys remain after reset all")
	}
}
