package httpserver

import (
	"encoding/json"
	"github.com/omniharmonic/studiolink/host/internal/buildinfo"
	"net/http/httptest"
	"testing"
)

func TestHealthIdentifiesExactProductAndBuild(t *testing.T) {
	response := httptest.NewRecorder()
	(&Server{}).handleHealth(response, httptest.NewRequest("GET", "/health", nil))
	var health map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || health["status"] != "ok" || health["product"] != buildinfo.Product || health["buildId"] != buildinfo.ID || health["buildId"] == "" {
		t.Fatalf("invalid health identity: %v", health)
	}
}
