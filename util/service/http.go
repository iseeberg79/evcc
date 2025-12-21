package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/evcc-io/evcc/plugin"
	"github.com/evcc-io/evcc/server/service"
	"github.com/evcc-io/evcc/util"
	"github.com/spf13/cast"
)

// Query contains all HTTP request parameters
type HTTPQuery struct {
	URI      string `mapstructure:"uri"`
	Method   string `mapstructure:"method"`
	Body     string `mapstructure:"body"`
	Headers  string `mapstructure:"headers"`
	Timeout  string `mapstructure:"timeout"`
	Insecure bool   `mapstructure:"insecure"`

	AuthType string `mapstructure:"authtype"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	Token    string `mapstructure:"token"`

	Jq         string  `mapstructure:"jq"`
	Regex      string  `mapstructure:"regex"`
	Decode     string  `mapstructure:"decode"`
	Scale      float64 `mapstructure:"scale"`
	ResultType string  `mapstructure:"resulttype"`
}

func init() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /read", httpRead)

	service.Register("http", mux)
}

// httpRead executes an HTTP request based on URL parameters
// Returns single value as array (for UI compatibility)
func httpRead(w http.ResponseWriter, req *http.Request) {
	// Convert URL query parameters to map for decoding
	cc := make(map[string]any)
	for k := range req.URL.Query() {
		cc[k] = req.URL.Query().Get(k)
	}

	// Decode query parameters into HTTPQuery struct with defaults
	query := HTTPQuery{
		Method: "GET",
		Scale:  1.0,
	}

	if err := util.DecodeOther(cc, &query); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}

	// Validate required parameters
	if query.URI == "" {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("uri parameter is required"))
		return
	}

	// Execute HTTP request via plugin
	value, err := executeHTTPRequest(context.TODO(), query)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}

	// Apply optional type cast
	if query.ResultType != "" {
		value = applyCast(value, query.ResultType)
	}

	jsonWrite(w, []string{cast.ToString(value)})
}

// executeHTTPRequest executes an HTTP request by reusing the HTTP plugin
func executeHTTPRequest(ctx context.Context, query HTTPQuery) (res any, err error) {
	// Build config map for plugin
	cfg := map[string]any{
		"uri":    query.URI,
		"method": query.Method,
		"scale":  query.Scale,
	}

	// Add body if present
	if query.Body != "" {
		cfg["body"] = query.Body
	}

	// Add timeout if present
	if query.Timeout != "" {
		cfg["timeout"] = query.Timeout
	}

	// Add insecure flag
	if query.Insecure {
		cfg["insecure"] = query.Insecure
	}

	// Parse and add headers
	if query.Headers != "" {
		headers, err := parseHTTPHeaders(query.Headers)
		if err != nil {
			return nil, fmt.Errorf("invalid headers: %w", err)
		}
		cfg["headers"] = headers
	}

	// Build Auth struct if authtype provided
	if query.AuthType != "" {
		auth := map[string]any{
			"type": query.AuthType,
		}
		if query.Username != "" {
			auth["user"] = query.Username
		}
		if query.Password != "" {
			auth["password"] = query.Password
		}
		if query.Token != "" {
			auth["token"] = query.Token
		}
		cfg["auth"] = auth
	}

	// Add pipeline processing parameters
	if query.Jq != "" {
		cfg["jq"] = query.Jq
	}
	if query.Regex != "" {
		cfg["regex"] = query.Regex
	}
	if query.Decode != "" {
		cfg["decode"] = query.Decode
	}

	// Create HTTP plugin instance
	p, err := plugin.NewHTTPPluginFromConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create http plugin: %w", err)
	}

	// Handle panics from plugin
	defer func() {
		if r := recover(); r != nil {
			res = nil
			err = fmt.Errorf("request failed: %v", r)
		}
	}()

	// Execute request using StringGetter
	getter, err := p.(plugin.StringGetter).StringGetter()
	if err != nil {
		return nil, err
	}

	return getter()
}

// parseHTTPHeaders converts "key:value,key:value" format to map
func parseHTTPHeaders(s string) (map[string]string, error) {
	headers := make(map[string]string)
	if s == "" {
		return headers, nil
	}

	pairs := strings.Split(s, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(pair, ":", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid header format: %s", pair)
		}
		headers[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return headers, nil
}
