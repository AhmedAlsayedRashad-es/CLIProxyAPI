package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8SoonestResetFirstStrategy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\nserver: {port: 8317}\nrouting: {strategy: round-robin}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PATCH("/v8/management/config", handler.ConfigV8)
	router.GET("/v8/management/config", handler.ConfigV8)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(`{"routing":{"strategy":"soonest-reset-first"}}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH: status=%d body=%s", response.Code, response.Body.String())
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.Strategy != "soonest-reset-first" {
		t.Fatalf("persisted strategy = %q, want soonest-reset-first", loaded.Routing.Strategy)
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"strategy":"soonest-reset-first"`) {
		t.Fatalf("GET: status=%d body=%s", response.Code, response.Body.String())
	}
}
