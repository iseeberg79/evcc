package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseHTTPHeaders(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
		wantErr  bool
	}{
		{
			name:     "empty string",
			input:    "",
			expected: map[string]string{},
			wantErr:  false,
		},
		{
			name:  "single header",
			input: "X-API-Key:abc123",
			expected: map[string]string{
				"X-API-Key": "abc123",
			},
			wantErr: false,
		},
		{
			name:  "multiple headers",
			input: "Content-Type:application/json,Accept:application/json",
			expected: map[string]string{
				"Content-Type": "application/json",
				"Accept":       "application/json",
			},
			wantErr: false,
		},
		{
			name:  "headers with spaces",
			input: "X-API-Key: abc123 , Accept: text/plain",
			expected: map[string]string{
				"X-API-Key": "abc123",
				"Accept":    "text/plain",
			},
			wantErr: false,
		},
		{
			name:     "invalid format - no colon",
			input:    "invalid",
			expected: nil,
			wantErr:  true,
		},
		{
			name:     "invalid format - multiple issues",
			input:    "valid:value,invalid",
			expected: nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHTTPHeaders(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestHTTPApplyCast(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		castType string
		expected any
	}{
		{
			name:     "int cast from string",
			value:    "42",
			castType: "int",
			expected: int64(42),
		},
		{
			name:     "int cast from float",
			value:    42.7,
			castType: "int",
			expected: int64(42),
		},
		{
			name:     "float cast from string",
			value:    "42.5",
			castType: "float",
			expected: float64(42.5),
		},
		{
			name:     "float cast from int",
			value:    42,
			castType: "float",
			expected: float64(42),
		},
		{
			name:     "string cast from int",
			value:    42,
			castType: "string",
			expected: "42",
		},
		{
			name:     "string cast from float",
			value:    42.5,
			castType: "float",
			expected: float64(42.5),
		},
		{
			name:     "no cast - unknown type",
			value:    "test",
			castType: "unknown",
			expected: "test",
		},
		{
			name:     "no cast - empty type",
			value:    42,
			castType: "",
			expected: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyCast(tt.value, tt.castType)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestGetHTTPParams_MissingURI(t *testing.T) {
	req := httptest.NewRequest("GET", "/params", nil)
	w := httptest.NewRecorder()

	getHTTPParams(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "uri parameter is required")
}

func TestGetHTTPParams_InvalidHeaders(t *testing.T) {
	req := httptest.NewRequest("GET", "/params?uri=https://example.com&headers=invalid", nil)
	w := httptest.NewRecorder()

	getHTTPParams(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "invalid header format")
}

func TestGetHTTPParams_SimpleGet(t *testing.T) {
	// Start a test HTTP server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("test-response"))
	}))
	defer ts.Close()

	req := httptest.NewRequest("GET", "/params?uri="+ts.URL, nil)
	w := httptest.NewRecorder()

	getHTTPParams(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "test-response")
}

func TestGetHTTPParams_WithHeaders(t *testing.T) {
	// Start a test HTTP server that checks headers
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") == "test123" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("authorized"))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer ts.Close()

	req := httptest.NewRequest("GET", "/params?uri="+ts.URL+"&headers=X-API-Key:test123", nil)
	w := httptest.NewRecorder()

	getHTTPParams(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "authorized")
}

func TestGetHTTPParams_POST(t *testing.T) {
	// Start a test HTTP server that checks method and body
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("post-response"))
		} else {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()

	req := httptest.NewRequest("GET", "/params?uri="+ts.URL+"&method=POST&body=test-body", nil)
	w := httptest.NewRecorder()

	getHTTPParams(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "post-response")
}
