// Package workbuddy implements browser authentication through CPA host callbacks.
package workbuddy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	provider    = "workbuddy"
	appVersion  = "5.9.3"
	cliVersion  = "2.165.0"
	loginWindow = 5 * time.Minute
	maxSessions = 128
)

// Error is safe to return across the plugin RPC boundary.
type Error struct {
	Code         string
	Message      string
	HTTPStatus   int
	UpstreamCode string
	RetryAfter   string
}

func (e *Error) Error() string { return e.Message }

func failure(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, HTTPStatus: status}
}

type site struct {
	id          string
	name        string
	endpoint    string
	appName     string
	modelPrefix string
}

func selectSite(id string) (site, bool) {
	switch id {
	case "workbuddy":
		return site{id: id, name: "WorkBuddy", endpoint: "https://copilot.tencent.com", appName: "WorkBuddy", modelPrefix: "wb/"}, true
	case "workbuddy-ai":
		return site{id: id, name: "WorkBuddy AI", endpoint: "https://www.workbuddy.ai", appName: "workbuddy-ai", modelPrefix: "wb-ai/"}, true
	default:
		return site{}, false
	}
}

// StartResponse and PollResponse retain the SDK's capitalized JSON fields.
type StartResponse struct {
	Provider  string
	URL       string
	State     string
	ExpiresAt time.Time
	Metadata  map[string]any
}

type PollResponse struct {
	Status  string
	Message string
	Auth    AuthData
	Auths   []AuthData
}

type loginRequest struct {
	Provider       string
	BaseURL        string
	State          string
	Metadata       map[string]any
	HostCallbackID string `json:"host_callback_id"`
}

type session struct {
	gate          chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
	timer         *time.Timer
	site          site
	upstreamState string
	token         *tokenData
}

// Service owns login sessions and admitted authentication, model, and execution
// work. Network transport and credential persistence belong to the CPA host.
type Service struct {
	mu            sync.Mutex
	closed        bool
	closeOnce     sync.Once
	sessions      map[string]*session
	execCtx       context.Context
	cancelExec    context.CancelFunc
	jobs          sync.WaitGroup
	refreshes     map[string]*refreshAccount
	compatibility chatCompatibility

	cfgMu        sync.RWMutex
	cfg          PluginConfig
	configPath   string
	hostCall     func(string, []byte) ([]byte, error)
	acctLocks    map[string]*sync.Mutex
	billingMu    sync.RWMutex
	billingCache map[string]AccountBillingSnapshot
	schedMu      sync.Mutex
	schedStarted bool
	schedWake    chan struct{}
	schedState   SchedulerStatus
	usage        *usageTracker
}

func New() *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		sessions:   make(map[string]*session),
		execCtx:    ctx,
		cancelExec: cancel,
		cfg:        DefaultPluginConfig(),
		configPath: DefaultConfigPath(),
		schedWake:  make(chan struct{}, 1),
		usage:      newUsageTracker(),
	}
	if s.configPath != "" {
		if loaded, ok, err := loadConfigFromFile(s.configPath); err == nil && ok {
			s.cfg = loaded
		}
	}
	if usagePath := DefaultUsagePath(); usagePath != "" {
		_ = s.usage.setPathAndLoad(usagePath)
	}
	return s
}

// Close stops admission, cancels login sessions and active work, and waits until
// every admitted operation has released its host callbacks and cached state.
// Concurrent and repeated calls wait for the same shutdown to finish.
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancelExec()
		for state, sess := range s.sessions {
			sess.cancel()
			sess.timer.Stop()
			delete(s.sessions, state)
		}
		s.mu.Unlock()
		s.jobs.Wait()
		s.mu.Lock()
		s.refreshes = nil
		s.mu.Unlock()
		s.compatibility.clear()
	})
}

// begin atomically admits work before Close can start waiting. The caller owns
// finish until its last synchronous callback or asynchronous stream pump exits.
func (s *Service) begin(ctx context.Context, timeout time.Duration) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, failure("service_closed", "WorkBuddy service is closed", 503)
	}
	s.jobs.Add(1)
	serviceCtx := s.execCtx
	s.mu.Unlock()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	stop := context.AfterFunc(serviceCtx, cancel)
	finish := func() {
		stop()
		cancel()
		s.jobs.Done()
	}
	return ctx, finish, nil
}

func decode(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, target) != nil {
		return failure("invalid_request", "request must be a valid JSON object", 400)
	}
	return nil
}

// Handle implements the schema-10 authentication methods. Every HTTP request
// uses the supplied host callback and the current request's host_callback_id.
func (s *Service) Handle(ctx context.Context, method string, raw []byte, call func(string, []byte) ([]byte, error)) (any, error) {
	switch method {
	case "executor.identifier", "executor.execute", "executor.execute_stream", "executor.count_tokens", "executor.http_request":
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, failure("service_closed", "WorkBuddy service is closed", 503)
		}
		return s.execute(ctx, method, raw, call)
	}
	ctx, finish, err := s.begin(ctx, 0)
	if err != nil {
		return nil, err
	}
	defer finish()
	if method == "auth.identifier" {
		var req struct{}
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return map[string]any{
			"identifier":           provider,
			"identifiers":          []string{"workbuddy", "workbuddy-ai"},
			"credential_providers": []string{provider},
			"entries": []map[string]string{
				{"identifier": "workbuddy", "name": "WorkBuddy国内"},
				{"identifier": "workbuddy-ai", "name": "WorkBuddy国际"},
			},
		}, nil
	}
	if method == "auth.parse" {
		return parse(raw)
	}
	if method == "auth.refresh" {
		return s.refresh(ctx, raw, call)
	}
	if method == "model.static" || method == "model.for_auth" {
		return s.models(ctx, method, raw, call)
	}
	if method == "quota.identifier" {
		return map[string]string{"identifier": provider}, nil
	}
	if method == "quota.describe" {
		return QuotaDescribeResponse{
			SupportedProviders: []string{provider},
			DisplayName:        "WorkBuddy",
			SupportsReset:      false,
		}, nil
	}
	if method == "quota.fetch" {
		return s.fetchQuotaRPC(ctx, raw, call)
	}
	if method == "quota.reset" {
		return nil, failure("unsupported_operation", "WorkBuddy quota reset is not supported", 501)
	}
	var req loginRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if _, ok := selectSite(req.Provider); !ok {
		return nil, failure("invalid_provider", "provider must be workbuddy or workbuddy-ai", 400)
	}
	switch method {
	case "auth.login.start":
		return s.start(ctx, req, call)
	case "auth.login.poll":
		return s.poll(ctx, req, call)
	default:
		return nil, failure("method_not_found", "unsupported authentication method", 404)
	}
}

func loginSite(entry string, metadata map[string]any) (site, error) {
	chosen, ok := selectSite(entry)
	if !ok {
		return site{}, failure("invalid_provider", "provider must be workbuddy or workbuddy-ai", 400)
	}
	for key := range metadata {
		if key != "site" && key != "is_webui" {
			return site{}, failure("invalid_metadata", "unsupported browser login metadata", 400)
		}
	}
	if value, present := metadata["site"]; present {
		id, ok := value.(string)
		if !ok || id != chosen.id {
			return site{}, failure("invalid_site", "metadata.site must match the selected WorkBuddy login entry", 400)
		}
	}
	return chosen, nil
}

func randomState() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", failure("random_failed", "cannot create secure login state", 500)
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Service) start(ctx context.Context, req loginRequest, call func(string, []byte) ([]byte, error)) (any, error) {
	chosen, err := loginSite(req.Provider, req.Metadata)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	full := len(s.sessions) >= maxSessions
	s.mu.Unlock()
	if full {
		return nil, failure("login_busy", "too many active browser logins; retry after one finishes", 429)
	}
	expires := time.Now().UTC().Add(loginWindow)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline.UTC()
	}
	var got struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	headers := map[string][]string{
		"X-No-Authorization":   {"true"},
		"X-No-User-Id":         {"true"},
		"X-No-Enterprise-Id":   {"true"},
		"X-No-Department-Info": {"true"},
	}
	if err := request(ctx, chosen, req.HostCallbackID, call, "POST", "/v2/plugin/auth/state?platform="+chosen.id, headers, []byte("{}"), &got); err != nil {
		return nil, publicUpstreamError(err)
	}
	if strings.TrimSpace(got.State) == "" {
		return nil, failure("invalid_upstream", "WorkBuddy returned no login state", 502)
	}
	page, err := url.Parse(got.AuthURL)
	if err != nil || !trustedLoginURL(chosen, page) {
		return nil, failure("untrusted_login_url", "WorkBuddy returned an untrusted login URL", 502)
	}
	local, err := randomState()
	if err != nil {
		return nil, err
	}
	browserSession, err := randomState()
	if err != nil {
		return nil, err
	}
	query := page.Query()
	query.Set("version", appVersion)
	query.Set("loginSessionId", browserSession)
	page.RawQuery = query.Encode()
	if ctx.Err() != nil || !time.Now().Before(expires) {
		return nil, failure("login_cancelled", "browser login was cancelled or expired", 408)
	}
	sessCtx, cancel := context.WithDeadline(context.Background(), expires)
	sess := &session{gate: make(chan struct{}, 1), ctx: sessCtx, cancel: cancel, site: chosen, upstreamState: got.State}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		cancel()
		return nil, failure("service_closed", "authentication service is closed", 503)
	}
	if len(s.sessions) >= maxSessions {
		cancel()
		return nil, failure("login_busy", "too many active browser logins; retry after one finishes", 429)
	}
	s.sessions[local] = sess
	sess.timer = time.AfterFunc(time.Until(expires), func() { s.discard(local, sess) })
	return StartResponse{Provider: chosen.id, URL: page.String(), State: local, ExpiresAt: expires, Metadata: map[string]any{"site": chosen.id}}, nil
}

func trustedLoginURL(chosen site, page *url.URL) bool {
	if page == nil || page.Scheme != "https" || page.User != nil || page.Opaque != "" || (page.Port() != "" && page.Port() != "443") {
		return false
	}
	host := strings.ToLower(page.Hostname())
	if chosen.id == "workbuddy-ai" {
		return host == "www.workbuddy.ai"
	}
	// The fixed enterprise sign-in sample uses the legacy CodeBuddy login
	// page; the current China browser flow uses www.workbuddy.cn.
	return host == "www.workbuddy.cn" || host == "www.codebuddy.cn"
}

func (s *Service) discard(state string, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[state] == sess {
		delete(s.sessions, state)
	}
	sess.cancel()
	if sess.timer != nil {
		sess.timer.Stop()
	}
}

func invalidSession() PollResponse {
	return PollResponse{Status: "error", Message: "login state is unknown, expired, cancelled, or already consumed"}
}

func (s *Service) poll(ctx context.Context, req loginRequest, call func(string, []byte) ([]byte, error)) (any, error) {
	s.mu.Lock()
	sess := s.sessions[req.State]
	s.mu.Unlock()
	if sess == nil {
		return invalidSession(), nil
	}
	chosen, err := loginSite(req.Provider, req.Metadata)
	if err != nil {
		return nil, err
	}
	if chosen.id != sess.site.id {
		return nil, failure("invalid_site", "poll provider must match the login site", 400)
	}
	select {
	case sess.gate <- struct{}{}:
	case <-ctx.Done():
		s.discard(req.State, sess)
		return invalidSession(), nil
	case <-sess.ctx.Done():
		return invalidSession(), nil
	}
	defer func() { <-sess.gate }()
	pollCtx, cancel := context.WithCancel(sess.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	cancelled := func() bool {
		select {
		case <-ctx.Done():
			cancel()
			return true
		case <-sess.ctx.Done():
			cancel()
			return true
		default:
			return false
		}
	}
	if cancelled() {
		s.discard(req.State, sess)
		return invalidSession(), nil
	}
	keep := false
	defer func() {
		if !keep || cancelled() {
			s.discard(req.State, sess)
			sess.token = nil
			sess.upstreamState = ""
		}
	}()
	pending := func(err error, code int) (any, error) {
		if cancelled() {
			return invalidSession(), nil
		}
		if transient(err, code) {
			keep = true
			return PollResponse{Status: "pending", Message: "waiting for browser sign-in"}, nil
		}
		return PollResponse{Status: "error", Message: "WorkBuddy browser sign-in failed"}, nil
	}
	query := "?state=" + url.QueryEscape(sess.upstreamState)
	if sess.token == nil {
		var token tokenData
		err := request(pollCtx, sess.site, req.HostCallbackID, call, "GET", "/v2/plugin/auth/token"+query, nil, nil, &token)
		if err != nil {
			return pending(err, 11217)
		}
		if cancelled() {
			return invalidSession(), nil
		}
		if token.AccessToken == "" {
			keep = true
			if token.empty() {
				return PollResponse{Status: "pending", Message: "waiting for browser sign-in"}, nil
			}
			keep = false
			return PollResponse{Status: "error", Message: "WorkBuddy returned invalid browser credentials"}, nil
		}
		if !token.valid() {
			return PollResponse{Status: "error", Message: "WorkBuddy returned invalid browser credentials"}, nil
		}
		token.resolveExpiry(time.Now().UTC())
		sess.token = &token
	}
	headers := map[string][]string{
		"Authorization":      {"Bearer " + sess.token.AccessToken},
		"X-Domain":           {requestDomain(sess.site, sess.token.Domain)},
		"X-No-User-Id":       {"true"},
		"X-No-Enterprise-Id": {"true"},
	}
	var who account
	if err := request(pollCtx, sess.site, req.HostCallbackID, call, "GET", "/v2/plugin/login/account"+query, headers, nil, &who); err != nil {
		return pending(err, 12151)
	}
	if cancelled() {
		return invalidSession(), nil
	}
	if who.UID == "" {
		if who.empty() {
			keep = true
			return PollResponse{Status: "pending", Message: "waiting for browser account selection"}, nil
		}
		return PollResponse{Status: "error", Message: "WorkBuddy returned no account identity"}, nil
	}
	ent, err := enterprise(pollCtx, sess.site, req.HostCallbackID, call, sess.token, who)
	if err != nil {
		return pending(err, 0)
	}
	if cancelled() {
		return invalidSession(), nil
	}
	auth, err := makeAuth(sess.site, *sess.token, who, ent)
	if err != nil {
		return PollResponse{Status: "error", Message: "WorkBuddy returned invalid browser credentials"}, nil
	}
	auth, err = preserveLoginFileName(auth, req.HostCallbackID, call)
	if err != nil {
		return nil, err
	}
	auth = s.captureModelSnapshot(pollCtx, auth, req.HostCallbackID, call)
	if cancelled() {
		return invalidSession(), nil
	}
	return PollResponse{Status: "success", Auth: auth}, nil
}

type hostRequest struct {
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers"`
	Body           []byte              `json:"body,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

type upstreamError struct {
	status       int
	code         int
	invalidToken bool
	authReason   string
}

func (e *upstreamError) Error() string { return "WorkBuddy request failed" }

func transient(err error, pendingCode int) bool {
	e, ok := err.(*upstreamError)
	return ok && (e.status == 408 || e.status == 429 || (pendingCode != 0 && e.code == pendingCode))
}

func publicUpstreamError(err error) error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return failure("upstream_error", "WorkBuddy browser request failed", 502)
}

func request(ctx context.Context, chosen site, callbackID string, call func(string, []byte) ([]byte, error), method, path string, headers map[string][]string, body []byte, target any) error {
	if ctx.Err() != nil {
		return failure("login_cancelled", "browser login was cancelled or expired", 408)
	}
	if call == nil {
		return failure("host_unavailable", "host HTTP callback is unavailable", 503)
	}
	if headers == nil {
		headers = make(map[string][]string)
	}
	headers["Content-Type"] = []string{"application/json"}
	headers["Accept"] = []string{"application/json"}
	if len(headers["User-Agent"]) == 0 {
		headers["User-Agent"] = []string{"WorkBuddy/" + appVersion}
	}
	raw, err := json.Marshal(hostRequest{Method: method, URL: chosen.endpoint + path, Headers: headers, Body: body, HostCallbackID: callbackID})
	if err != nil {
		return failure("internal_error", "cannot encode host HTTP request", 500)
	}
	reply, err := call("host.http.do", raw)
	if ctx.Err() != nil {
		return failure("login_cancelled", "browser login was cancelled or expired", 408)
	}
	if err != nil {
		return failure("host_http_failed", "host HTTP request failed", 502)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int
			Headers    map[string][]string
			Body       []byte
		} `json:"result"`
	}
	if json.Unmarshal(reply, &envelope) != nil || !envelope.OK || envelope.Result.StatusCode < 100 || envelope.Result.StatusCode > 599 {
		return failure("host_http_failed", "host returned an invalid HTTP response", 502)
	}
	var data struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	status := envelope.Result.StatusCode
	authReason := authenticationReason(status, envelope.Result.Headers, envelope.Result.Body)
	if status == 401 || status >= 500 || authReason != "" {
		return &upstreamError{status: status, invalidToken: authReason == "invalid_token", authReason: authReason}
	}
	if status == 408 || status == 429 {
		return &upstreamError{status: status}
	}
	if json.Unmarshal(envelope.Result.Body, &data) != nil {
		return failure("invalid_upstream", "WorkBuddy returned an invalid response", 502)
	}
	code := 0
	if len(data.Code) != 0 && string(data.Code) != "null" {
		value := strings.Trim(string(data.Code), "\"")
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return failure("invalid_upstream", "WorkBuddy returned an invalid response code", 502)
		}
		code = parsed
	}
	if status < 200 || status >= 300 || code != 0 {
		return &upstreamError{status: status, code: code, authReason: authReason}
	}
	if len(data.Data) == 0 || string(data.Data) == "null" {
		return nil
	}
	if json.Unmarshal(data.Data, target) != nil {
		return failure("invalid_upstream", "WorkBuddy returned invalid authentication data", 502)
	}
	payload := bytes.TrimSpace(data.Data)
	emptyObject := len(payload) >= 2 && payload[0] == '{' && payload[len(payload)-1] == '}' && len(bytes.TrimSpace(payload[1:len(payload)-1])) == 0
	if !emptyObject {
		switch value := target.(type) {
		case *tokenData:
			if value.AccessToken == "" {
				return failure("invalid_upstream", "WorkBuddy returned no access token", 502)
			}
		case *account:
			if strings.TrimSpace(value.UID) == "" {
				return failure("invalid_upstream", "WorkBuddy returned no account identity", 502)
			}
		}
	}
	return nil
}
