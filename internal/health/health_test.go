package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerReturnsStatusOK(t *testing.T) {
	handler := Handler("docker")
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
}

func TestHandlerResponseStructure(t *testing.T) {
	handler := Handler("docker")
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	var resp Response
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "healthy", resp.Status)
	assert.Equal(t, "scaleset", resp.ServiceName)
	assert.Equal(t, "docker", resp.Engine)
	assert.NotEmpty(t, resp.Version)
	assert.NotEmpty(t, resp.Commit)
	assert.NotEmpty(t, resp.BuildTime)
	assert.NotEmpty(t, resp.GoVersion)
	assert.NotEmpty(t, resp.OS)
	assert.NotEmpty(t, resp.Architecture)
	assert.False(t, resp.Timestamp.IsZero())
}

func TestHandlerWithDifferentEngines(t *testing.T) {
	engines := []string{"docker", "gcp", "aws", "azure"}

	for _, eng := range engines {
		t.Run(eng, func(t *testing.T) {
			handler := Handler(eng)
			req := httptest.NewRequest("GET", "/healthz", nil)
			w := httptest.NewRecorder()

			handler(w, req)

			var resp Response
			err := json.Unmarshal(w.Body.Bytes(), &resp)
			require.NoError(t, err)

			assert.Equal(t, eng, resp.Engine)
		})
	}
}

func TestHandlerResponseIsValidJSON(t *testing.T) {
	handler := Handler("docker")
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	// Should be valid JSON that can be unmarshaled
	var resp Response
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Round-trip test: re-encode and check it's still valid
	reencoded, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.NotEmpty(t, reencoded)
}

func TestHandlerHTTPMethod(t *testing.T) {
	handler := Handler("docker")

	t.Run("GET", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	// Handler should work for any method (no method checking)
	t.Run("POST", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("HEAD", func(t *testing.T) {
		req := httptest.NewRequest("HEAD", "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestHandlerResponseBody(t *testing.T) {
	handler := Handler("docker")
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	// Check response is not empty
	assert.Greater(t, w.Body.Len(), 0)

	// Check response contains expected JSON fields
	body := w.Body.String()
	assert.True(t, strings.Contains(body, "healthy"))
	assert.True(t, strings.Contains(body, "scaleset"))
	assert.True(t, strings.Contains(body, "docker"))
	assert.True(t, strings.Contains(body, "go_version"))
}
