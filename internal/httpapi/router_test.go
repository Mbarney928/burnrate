package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mbarney928/burnrate/internal/auth"
	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/ratelimit"
	"github.com/Mbarney928/burnrate/internal/service"
	"github.com/Mbarney928/burnrate/internal/store"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

type env struct {
	srv          *httptest.Server
	editor       string
	viewer       string
	logs         *bytes.Buffer
	healthErr    error
	authFailures *ratelimit.Limiter
}

type options struct {
	rps, burst   int
	authFailures int
	tokens       TokenLookup
}

func newEnv(t *testing.T, opt options) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	e := &env{logs: &bytes.Buffer{}}
	for _, tc := range []struct {
		name string
		role model.Role
		dst  *string
	}{{"test-editor", model.RoleEditor, &e.editor}, {"test-viewer", model.RoleViewer, &e.viewer}} {
		plain, hash, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateToken(ctx, tc.name, tc.role, hash); err != nil {
			t.Fatal(err)
		}
		*tc.dst = plain
	}

	if opt.rps == 0 {
		opt.rps, opt.burst = 1000, 1000
	}
	if opt.authFailures == 0 {
		opt.authFailures = 100
	}
	var tokens TokenLookup = st
	if opt.tokens != nil {
		tokens = opt.tokens
	}
	e.authFailures = ratelimit.New(float64(opt.authFailures)/60, opt.authFailures, nil)
	h := NewRouter(Deps{
		Service:      service.New(st, func() time.Time { return now }),
		Tokens:       tokens,
		Health:       func(context.Context) error { return e.healthErr },
		OpenAPI:      []byte("openapi: 3.1.0\n"),
		Logger:       slog.New(slog.NewJSONHandler(e.logs, nil)),
		Limiter:      ratelimit.New(float64(opt.rps), opt.burst, nil),
		AuthFailures: e.authFailures,
	})
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	return e
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (e *env) do(t *testing.T, method, path, token, body string) resp {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, header: res.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func errCode(r resp) string {
	if e, ok := r.body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func expect(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && errCode(r) != code) {
		t.Fatalf("got %d %s, want %d %s; body %s", r.status, errCode(r), status, code, r.raw)
	}
}

func TestPublicEndpointsAndSecurityHeaders(t *testing.T) {
	e := newEnv(t, options{})
	r := e.do(t, http.MethodGet, "/healthz", "", "")
	expect(t, r, http.StatusOK, "")
	if r.body["status"] != "ok" {
		t.Errorf("healthz body = %s", r.raw)
	}
	for h, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := r.header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if r.header.Get("X-Request-Id") == "" && !strings.Contains(e.logs.String(), `"request_id"`) {
		t.Error("requests should carry a request ID in logs")
	}

	e.healthErr = errors.New("db down")
	expect(t, e.do(t, http.MethodGet, "/healthz", "", ""), http.StatusServiceUnavailable, "")

	r = e.do(t, http.MethodGet, "/openapi.yaml", "", "")
	if r.status != http.StatusOK || !strings.HasPrefix(r.raw, "openapi:") || r.header.Get("Content-Type") != "application/yaml" {
		t.Errorf("openapi = %d %q", r.status, r.raw)
	}
	expect(t, e.do(t, http.MethodGet, "/nope", "", ""), http.StatusNotFound, "not_found")
	expect(t, e.do(t, http.MethodDelete, "/healthz", "", ""), http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestAuthenticationAndRoles(t *testing.T) {
	e := newEnv(t, options{})
	expect(t, e.do(t, http.MethodGet, "/api/v1/services", "", ""), http.StatusUnauthorized, "unauthorized")
	expect(t, e.do(t, http.MethodGet, "/api/v1/services", "not-a-token", ""), http.StatusUnauthorized, "unauthorized")
	forged, _, _ := auth.NewToken()
	r := e.do(t, http.MethodGet, "/api/v1/services", forged, "")
	expect(t, r, http.StatusUnauthorized, "unauthorized")
	if r.header.Get("WWW-Authenticate") == "" {
		t.Error("401 should include WWW-Authenticate")
	}

	expect(t, e.do(t, http.MethodGet, "/api/v1/services", e.viewer, ""), http.StatusOK, "")
	expect(t, e.do(t, http.MethodPost, "/api/v1/services", e.viewer, `{"name":"x-api","team":"t"}`), http.StatusForbidden, "forbidden")
	expect(t, e.do(t, http.MethodPost, "/api/v1/services", e.editor, `{"name":"x-api","team":"t"}`), http.StatusCreated, "")

	for _, secret := range []string{e.editor, e.viewer, forged} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatal("a token was written to the logs")
		}
	}
}

func TestFailedAuthIsRateLimited(t *testing.T) {
	e := newEnv(t, options{authFailures: 3})
	for range 3 {
		expect(t, e.do(t, http.MethodGet, "/api/v1/status", "bad", ""), http.StatusUnauthorized, "unauthorized")
	}
	r := e.do(t, http.MethodGet, "/api/v1/status", e.viewer, "")
	expect(t, r, http.StatusTooManyRequests, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Error("429 should include Retry-After")
	}
}

func TestGeneralRateLimit(t *testing.T) {
	e := newEnv(t, options{rps: 1, burst: 2})
	expect(t, e.do(t, http.MethodGet, "/healthz", "", ""), http.StatusOK, "")
	expect(t, e.do(t, http.MethodGet, "/healthz", "", ""), http.StatusOK, "")
	expect(t, e.do(t, http.MethodGet, "/healthz", "", ""), http.StatusTooManyRequests, "rate_limited")
}

func TestEndToEndIngestAndStatus(t *testing.T) {
	e := newEnv(t, options{})
	expect(t, e.do(t, http.MethodPost, "/api/v1/services", e.editor,
		`{"name":"search-api","team":"discovery","description":"Fictional product search"}`), http.StatusCreated, "")
	expect(t, e.do(t, http.MethodPost, "/api/v1/services", e.editor, `{"name":"search-api","team":"x"}`), http.StatusConflict, "conflict")
	expect(t, e.do(t, http.MethodGet, "/api/v1/services/search-api", e.viewer, ""), http.StatusOK, "")
	expect(t, e.do(t, http.MethodGet, "/api/v1/services/missing", e.viewer, ""), http.StatusNotFound, "not_found")

	r := e.do(t, http.MethodPost, "/api/v1/services/search-api/slos", e.editor, `{"name":"availability","target":0.99,"window_days":30}`)
	expect(t, r, http.StatusCreated, "")
	id := int64(r.body["id"].(float64))
	expect(t, e.do(t, http.MethodGet, "/api/v1/services/search-api/slos", e.viewer, ""), http.StatusOK, "")
	expect(t, e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/slos/%d", id), e.viewer, ""), http.StatusOK, "")
	expect(t, e.do(t, http.MethodGet, "/api/v1/slos/0", e.viewer, ""), http.StatusBadRequest, "validation_failed")
	expect(t, e.do(t, http.MethodGet, "/api/v1/slos/abc/status", e.viewer, ""), http.StatusBadRequest, "validation_failed")
	expect(t, e.do(t, http.MethodPost, "/api/v1/slos/x/events", e.editor, `{}`), http.StatusBadRequest, "validation_failed")

	// An hour of 25% errors against a 99% target is a burn rate of 25: page.
	var events []string
	for m := range 60 {
		ts := now.Add(-time.Duration(m) * time.Minute).Format(time.RFC3339)
		events = append(events, fmt.Sprintf(`{"timestamp":%q,"good":75,"total":100}`, ts))
	}
	r = e.do(t, http.MethodPost, fmt.Sprintf("/api/v1/slos/%d/events", id), e.editor, `{"events":[`+strings.Join(events, ",")+`]}`)
	expect(t, r, http.StatusAccepted, "")
	if r.body["accepted"] != float64(60) {
		t.Errorf("accepted = %v", r.body["accepted"])
	}

	r = e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/slos/%d/status", id), e.viewer, "")
	expect(t, r, http.StatusOK, "")
	status := r.body["status"].(map[string]any)
	if status["severity"] != "page" {
		t.Errorf("severity = %v; body %s", status["severity"], r.raw)
	}

	// Point-in-time query from before the incident: nothing to report.
	r = e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/slos/%d/status?at=%s", id, now.Add(-2*time.Hour).Format(time.RFC3339)), e.viewer, "")
	expect(t, r, http.StatusOK, "")
	if r.body["status"].(map[string]any)["severity"] != "none" {
		t.Errorf("historical severity = %s", r.raw)
	}
	expect(t, e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/slos/%d/status?at=yesterday", id), e.viewer, ""), http.StatusBadRequest, "validation_failed")
	expect(t, e.do(t, http.MethodGet, "/api/v1/slos/999/status", e.viewer, ""), http.StatusNotFound, "not_found")

	r = e.do(t, http.MethodGet, "/api/v1/status", e.viewer, "")
	expect(t, r, http.StatusOK, "")
	if n := len(r.body["slos"].([]any)); n != 1 {
		t.Errorf("overview has %d SLOs", n)
	}
	expect(t, e.do(t, http.MethodGet, "/api/v1/status?at=bad", e.viewer, ""), http.StatusBadRequest, "validation_failed")
}

func TestInputValidationAndJSONHandling(t *testing.T) {
	e := newEnv(t, options{})
	r := e.do(t, http.MethodPost, "/api/v1/services", e.editor, `{"name":"Bad!","team":""}`)
	expect(t, r, http.StatusBadRequest, "validation_failed")
	fields := r.body["error"].(map[string]any)["fields"].(map[string]any)
	if fields["name"] == nil || fields["team"] == nil {
		t.Errorf("fields = %v", fields)
	}

	for _, body := range []string{
		`not json`,
		`{"name":"a-api","team":"t","admin":true}`, // unknown field
		`{"name":"a-api","team":"t"} {"x":1}`,      // trailing data
		`[]`,
	} {
		expect(t, e.do(t, http.MethodPost, "/api/v1/services", e.editor, body), http.StatusBadRequest, "invalid_json")
	}
	huge := `{"name":"a-api","team":"t","description":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	expect(t, e.do(t, http.MethodPost, "/api/v1/services", e.editor, huge), http.StatusRequestEntityTooLarge, "body_too_large")

	expect(t, e.do(t, http.MethodPost, "/api/v1/services/none/slos", e.editor, `{"name":"availability","target":0.99,"window_days":30}`), http.StatusNotFound, "not_found")
	expect(t, e.do(t, http.MethodPost, "/api/v1/services/none/slos", e.editor, `{"name":`), http.StatusBadRequest, "invalid_json")
	expect(t, e.do(t, http.MethodPost, "/api/v1/slos/1/events", e.editor, `{"events":"x"}`), http.StatusBadRequest, "invalid_json")
	expect(t, e.do(t, http.MethodPost, "/api/v1/slos/1/events", e.editor, `{"events":[]}`), http.StatusNotFound, "not_found")
}

type brokenTokens struct{}

func (brokenTokens) TokenByHash(context.Context, string) (model.Token, error) {
	return model.Token{}, errors.New("database is locked")
}

func TestInternalErrorsAreGeneric(t *testing.T) {
	e := newEnv(t, options{tokens: brokenTokens{}})
	r := e.do(t, http.MethodGet, "/api/v1/status", e.viewer, "")
	expect(t, r, http.StatusInternalServerError, "internal_error")
	if strings.Contains(r.raw, "locked") {
		t.Error("internal error details leaked to the client")
	}
	if !strings.Contains(e.logs.String(), "database is locked") {
		t.Error("internal error should be logged server-side")
	}
}

func TestRecovererHidesPanics(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom: secret detail") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("recoverer response = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Error("panic should be logged")
	}

	abort := recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if err, _ := recover().(error); !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("ErrAbortHandler should be re-panicked, got %v", err)
		}
	}()
	abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestClientIPFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.10"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := clientIP(r); got != "192.0.2.10" {
		t.Errorf("clientIP = %q", got)
	}
	r.RemoteAddr = "192.0.2.10:5000"
	if got := clientIP(r); got != "192.0.2.10" {
		t.Errorf("clientIP = %q", got)
	}
}

func TestRequireWriterWithoutToken(t *testing.T) {
	rec := httptest.NewRecorder()
	requireWriter(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d", rec.Code)
	}
}
