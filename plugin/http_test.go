package plugin

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type httpHandler struct {
	val          string
	req          *http.Request
	cnt          int
	cacheBusting bool
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.req = req
	h.val = lo.RandomString(16, lo.LettersCharset)
	if h.cacheBusting {
		w.Header().Set("Cache-Control", "no-store, no-cache, max-age=0, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
	}
	_, _ = w.Write([]byte(h.val))
	h.cnt++
}

func TestHttp(t *testing.T) {
	suite.Run(t, new(httpTestSuite))
}

type httpTestSuite struct {
	suite.Suite
	h   *httpHandler
	srv *httptest.Server
}

func (suite *httpTestSuite) SetupSuite() {
	suite.h = new(httpHandler)
	suite.srv = httptest.NewServer(suite.h)
}

func (suite *httpTestSuite) TearDown() {
	suite.srv.Close()
}

func (suite *httpTestSuite) TestGet() {
	uri := suite.srv.URL + "/foo/bar{{\"/baz\"}}"
	p := NewHTTP(util.NewLogger("foo"), http.MethodGet, uri, false, 0)

	g, err := p.StringGetter()
	suite.Require().NoError(err)

	res, err := g()
	suite.Require().NoError(err)
	suite.Require().Equal("/foo/bar/baz", suite.h.req.URL.String())
	suite.Require().Equal(suite.h.val, res)
}

func (suite *httpTestSuite) TestCacheGet() {
	uri := suite.srv.URL + "/foo/bar?baz=1"
	p := NewHTTP(util.NewLogger("foo"), http.MethodGet, uri, false, time.Minute)

	g, err := p.StringGetter()
	suite.Require().NoError(err)

	for range 3 {
		res, err := g()
		suite.Require().NoError(err)
		suite.Require().Equal("/foo/bar?baz=1", suite.h.req.URL.String())
		suite.Require().Equal(suite.h.val, res)
		suite.Require().Equal(1, suite.h.cnt)
	}
}

func (suite *httpTestSuite) TestCacheGetNoStore() {
	// upstream sends cache-busting headers, cache must still take effect (#31025)
	suite.h.cacheBusting = true
	defer func() { suite.h.cacheBusting = false }()

	uri := suite.srv.URL + "/foo/bar?baz=2"
	p := NewHTTP(util.NewLogger("foo"), http.MethodGet, uri, false, time.Minute)

	g, err := p.StringGetter()
	suite.Require().NoError(err)

	suite.h.cnt = 0
	res, err := g()
	suite.Require().NoError(err)
	first := suite.h.cnt

	for range 3 {
		val, err := g()
		suite.Require().NoError(err)
		suite.Require().Equal(res, val)
		suite.Require().Equal(first, suite.h.cnt)
	}
}

func (suite *httpTestSuite) TestSetQuery() {
	uri := suite.srv.URL + "/foo/bar?baz={{.baz}}"
	p := NewHTTP(util.NewLogger("foo"), http.MethodGet, uri, false, 0)

	s, err := p.StringSetter("baz")
	suite.Require().NoError(err)
	suite.Require().NoError(s("4711"))
	suite.Require().Equal("/foo/bar?baz=4711", suite.h.req.URL.String())
}

func (suite *httpTestSuite) TestSetPath() {
	uri := suite.srv.URL + "/foo/bar/{{.baz}}"
	p := NewHTTP(util.NewLogger("foo"), http.MethodGet, uri, false, 0)

	s, err := p.StringSetter("baz")
	suite.Require().NoError(err)
	suite.Require().NoError(s("4711"))
	suite.Require().Equal("/foo/bar/4711", suite.h.req.URL.String())
}

// forwardServer is a test http server that serves a dynamic value on /value and
// records values written to /write. It models the nested-setter chain where an
// http plugin reads a value (e.g. from /api/state) and forwards it to a nested
// setter (e.g. modbus).
type forwardServer struct {
	*httptest.Server
	mu      sync.Mutex
	value   string
	written []string
}

func newForwardServer(value string) *forwardServer {
	s := &forwardServer{value: value}

	mux := http.NewServeMux()
	mux.HandleFunc("/value", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, _ = w.Write([]byte(s.value))
	})
	mux.HandleFunc("/write", func(_ http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.written = append(s.written, r.URL.Query().Get("v"))
	})

	s.Server = httptest.NewServer(mux)
	return s
}

func (s *forwardServer) setValue(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = v
}

func (s *forwardServer) writes() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := make([]float64, 0, len(s.written))
	for _, w := range s.written {
		f, _ := strconv.ParseFloat(w, 64)
		res = append(res, f)
	}
	return res
}

// newForwardingHTTP builds an http plugin that reads from /value and forwards
// the value to a nested http setter writing to /write?v={{.foo}}.
func newForwardingHTTP(t *testing.T, srv *forwardServer) Plugin {
	t.Helper()
	p, err := NewHTTPPluginFromConfig(t.Context(), map[string]any{
		"uri": srv.URL + "/value",
		"set": map[string]any{
			"source": "http",
			"uri":    srv.URL + "/write?v={{.foo}}",
		},
	})
	require.NoError(t, err)
	return p
}

// TestIntSetterForward verifies the nested-setter path: the incoming int is
// ignored, the current value is read via GET and forwarded to the nested setter.
func TestIntSetterForward(t *testing.T) {
	srv := newForwardServer("1500")
	defer srv.Close()

	set, err := newForwardingHTTP(t, srv).(IntSetter).IntSetter("foo")
	require.NoError(t, err)

	// incoming value (99) is ignored; current GET value (1500) is forwarded
	require.NoError(t, set(99))
	require.Equal(t, []float64{1500}, srv.writes())

	// a changed value is picked up on the next call (the dynamic behaviour the
	// watchdog relies on for periodic re-writes)
	srv.setValue("2000")
	require.NoError(t, set(99))
	require.Equal(t, []float64{1500, 2000}, srv.writes())
}

// TestFloatSetterForward verifies the symmetric nested-setter path for floats.
func TestFloatSetterForward(t *testing.T) {
	srv := newForwardServer("1500")
	defer srv.Close()

	set, err := newForwardingHTTP(t, srv).(FloatSetter).FloatSetter("foo")
	require.NoError(t, err)

	require.NoError(t, set(99))
	require.Equal(t, []float64{1500}, srv.writes())

	srv.setValue("2000")
	require.NoError(t, set(99))
	require.Equal(t, []float64{1500, 2000}, srv.writes())
}

func (suite *httpTestSuite) TestNoCacheClockSkew() {
	// no cache configured, no cache headers, response header date with +2s clock skew
	// validate future value (currentAge<0) is not handled as fresh value from cache
	suite.h.dateSkew = 2 * time.Second
	defer func() { suite.h.dateSkew = 0 }()

	uri := suite.srv.URL + "/foo/bar?baz=5"
	p := NewHTTP(util.NewLogger("foo"), http.MethodGet, uri, false, 0)
	g, err := p.StringGetter()
	suite.Require().NoError(err)

	suite.h.cnt = 0
	first, err := g()
	suite.Require().NoError(err)
	second, err := g()
	suite.Require().NoError(err)

	suite.Require().NotEqual(first, second)
	suite.Require().Equal(2, suite.h.cnt)
}
