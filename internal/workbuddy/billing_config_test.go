package workbuddy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-workbuddy/internal/workbuddy"
)

func TestConfigYAMLAndOptionAModelPrefixRouting(t *testing.T) {
	t.Setenv(workbuddy.ConfigPathEnv, filepath.Join(t.TempDir(), "config.json"))
	s := workbuddy.New()
	defer s.Close()

	// Default config enables both prefixes.
	cfg := s.Config()
	if !cfg.CNModelPrefix || !cfg.IntlModelPrefix || !cfg.AutoCheckinEnabled || cfg.AutoCheckinTime != "09:00" {
		t.Fatalf("unexpected default config: %+v", cfg)
	}

	// Disable CN prefix only; keep Intl prefix enabled.
	if err := s.Reconfigure([]byte("cn_model_prefix: false\nauto_checkin_time: \"08:30\"\n")); err != nil {
		t.Fatal(err)
	}
	cfg = s.Config()
	if cfg.CNModelPrefix || !cfg.IntlModelPrefix || !cfg.AutoCheckinEnabled || cfg.AutoCheckinTime != "08:30" {
		t.Fatalf("unexpected reconfigured values: %+v", cfg)
	}

	// Invalid time must fail loudly.
	if err := s.Reconfigure([]byte("auto_checkin_time: \"25:99\"\n")); err == nil {
		t.Fatal("expected invalid auto_checkin_time to fail")
	}

	// Verify model discovery omits wb/ for CN while preserving wb-ai/ for Intl.
	cnAuth := parsedIdentity(t, "workbuddy", "")
	intlAuth := parsedIdentity(t, "workbuddy-ai", "")
	const catalogCfg = `{"agents":[{"name":"cli","models":["glm-5.3"]}],"models":[{"id":"glm-5.3","name":"GLM-5.3"}]}`
	discoverOne := func(auth workbuddy.AuthData) []string {
		reqRaw, _ := json.Marshal(map[string]any{
			"AuthID": auth.ID, "AuthProvider": auth.Provider, "StorageJSON": auth.StorageJSON,
			"Metadata": auth.Metadata, "host_callback_id": "model-cb",
		})
		out, err := s.Handle(context.Background(), "model.for_auth", reqRaw, func(method string, raw []byte) ([]byte, error) {
			switch method {
			case "host.http.do":
				return json.Marshal(map[string]any{"ok": true, "result": map[string]any{
					"StatusCode": 200,
					"Headers":    map[string][]string{"Content-Type": {"application/json"}},
					"Body":       []byte(`{"code":0,"data":` + catalogCfg + `}`),
				}})
			case "host.log":
				return []byte(`{"ok":true}`), nil
			default:
				return nil, nil
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		res := out.(workbuddy.ModelResponse)
		ids := make([]string, 0, len(res.Models))
		for _, m := range res.Models {
			ids = append(ids, m.ID)
		}
		return ids
	}

	if got := discoverOne(cnAuth); !slices.Equal(got, []string{"glm-5.3"}) {
		t.Fatalf("CN models with prefix disabled = %v, want [glm-5.3]", got)
	}
	if got := discoverOne(intlAuth); !slices.Equal(got, []string{"wb-ai/glm-5.3"}) {
		t.Fatalf("Intl models with prefix enabled = %v, want [wb-ai/glm-5.3]", got)
	}

	// Option A backward compatibility: CN accepts BOTH unprefixed glm-5.3 AND wb/glm-5.3,
	// while still rejecting cross-site wb-ai/glm-5.3 and nested wb/wb/glm-5.3.
	for _, validCN := range []string{"glm-5.3", "wb/glm-5.3"} {
		sent := executeRequestProtocol(t, s, requestProtocolInput(cnAuth, validCN, []byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
		body := requestProtocolBody(t, sent)
		if string(body["model"]) != `"glm-5.3"` {
			t.Fatalf("CN %q upstream model = %s, want \"glm-5.3\"", validCN, body["model"])
		}
	}
	for _, rejected := range []struct {
		auth  workbuddy.AuthData
		model string
	}{
		{cnAuth, "wb-ai/glm-5.3"},
		{cnAuth, "wb/wb/glm-5.3"},
		{intlAuth, "glm-5.3"}, // Intl prefix is still enabled, so unprefixed is rejected on Intl
		{intlAuth, "wb/glm-5.3"},
	} {
		_, err := invoke(s, context.Background(), "executor.execute", requestProtocolInput(rejected.auth, rejected.model, []byte(`{"messages":[{"role":"user","content":"hi"}]}`)), nil)
		if err == nil {
			t.Fatalf("expected %s to reject %q", rejected.auth.Attributes["site"], rejected.model)
		}
	}
}

func TestStandaloneJSONConfigPersistence(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nested", "config.json")
	t.Setenv(workbuddy.ConfigPathEnv, cfgPath)

	s1 := workbuddy.New()
	defer s1.Close()

	want := workbuddy.PluginConfig{
		CNModelPrefix:      false,
		IntlModelPrefix:    true,
		AutoCheckinEnabled: false,
		AutoCheckinTime:    "07:45",
	}
	if err := s1.UpdateConfig(want); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("expected persisted config file at %s: %v", cfgPath, err)
	}

	// New service instance loads persisted JSON config on startup.
	s2 := workbuddy.New()
	defer s2.Close()
	if got := s2.Config(); got != want {
		t.Fatalf("reloaded config mismatch: got %+v, want %+v", got, want)
	}

	// Host Reconfigure with different YAML should not overwrite persisted JSON config.
	if err := s2.Reconfigure([]byte("cn_model_prefix: true\nauto_checkin_time: \"10:15\"\n")); err != nil {
		t.Fatalf("Reconfigure failed: %v", err)
	}
	if got := s2.Config(); got != want {
		t.Fatalf("Reconfigure should preserve persisted JSON config: got %+v, want %+v", got, want)
	}
}

type fakeHostHarness struct {
	mu      sync.Mutex
	files   []map[string]any
	authMap map[string]map[string]any
	saved   map[string]json.RawMessage
	calls   []string
	handler func(method, url string, body []byte) (int, []byte)
	// callOverride lets a test fail a specific host method. handled=false falls
	// through to the default fake behaviour.
	callOverride func(method string) (reply []byte, err error, handled bool)
}

func newFakeHostHarness(t *testing.T, auths []workbuddy.AuthData) *fakeHostHarness {
	t.Helper()
	h := &fakeHostHarness{
		authMap: make(map[string]map[string]any),
		saved:   make(map[string]json.RawMessage),
	}
	for i, auth := range auths {
		idx := auth.Attributes["site"] + "-" + string(rune('a'+i))
		fileName := auth.FileName
		if fileName == "" {
			fileName = idx + ".json"
		}
		var rawMap map[string]any
		if err := json.Unmarshal(auth.StorageJSON, &rawMap); err != nil {
			t.Fatal(err)
		}
		h.files = append(h.files, map[string]any{
			"id":         auth.ID,
			"auth_index": idx,
			"name":       fileName,
			"provider":   "workbuddy",
			"type":       "workbuddy",
			"label":      auth.Label,
		})
		h.authMap[idx] = map[string]any{
			"auth_index": idx,
			"name":       fileName,
			"path":       "/tmp/" + fileName,
			"json":       rawMap,
		}
	}
	return h
}

func (h *fakeHostHarness) call(method string, raw []byte) ([]byte, error) {
	h.mu.Lock()
	h.calls = append(h.calls, method)
	override := h.callOverride
	h.mu.Unlock()
	if override != nil {
		if reply, err, handled := override(method); handled {
			return reply, err
		}
	}

	switch method {
	case "host.auth.list":
		h.mu.Lock()
		files := slices.Clone(h.files)
		h.mu.Unlock()
		return json.Marshal(map[string]any{"ok": true, "result": map[string]any{"files": files}})
	case "host.auth.get":
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		h.mu.Lock()
		rec, ok := h.authMap[req.AuthIndex]
		h.mu.Unlock()
		if !ok {
			return json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": "not_found", "message": "not found"}})
		}
		return json.Marshal(map[string]any{"ok": true, "result": rec})
	case "host.auth.save":
		var req struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		var updatedMap map[string]any
		_ = json.Unmarshal(req.JSON, &updatedMap)
		h.mu.Lock()
		h.saved[req.Name] = slices.Clone(req.JSON)
		if updatedMap != nil {
			for _, rec := range h.authMap {
				if rec["name"] == req.Name {
					rec["json"] = updatedMap
				}
			}
		}
		h.mu.Unlock()
		return json.Marshal(map[string]any{"ok": true, "result": map[string]any{"name": req.Name, "path": "/tmp/" + req.Name}})
	case "host.log":
		return []byte(`{"ok":true}`), nil
	case "host.http.do":
		var req struct {
			Method string `json:"method"`
			URL    string `json:"url"`
			Body   []byte `json:"body"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		status, body := h.handler(req.Method, req.URL, req.Body)
		return json.Marshal(map[string]any{"ok": true, "result": map[string]any{
			"StatusCode": status,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       body,
		}})
	default:
		return nil, nil
	}
}

func TestCNCheckinGlobalDailyBonusAndQuotaRPC(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()

	cnAuth := parsedIdentity(t, "workbuddy", "")
	intlAuth := parsedIdentity(t, "workbuddy-ai", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth, intlAuth})

	var reportedEvents []map[string]any
	checkinCalled := 0
	intlLit := false

	h.handler = func(method, reqURL string, body []byte) (int, []byte) {
		switch {
		case strings.HasSuffix(reqURL, "/v2/billing/meter/checkin-activity-status"):
			return 200, []byte(`{"code":0,"data":{"active":true,"today_checked_in":false,"daily_credit":100,"streak_days":4}}`)
		case strings.HasSuffix(reqURL, "/v2/billing/meter/daily-checkin"):
			checkinCalled++
			return 200, []byte(`{"code":0,"data":{"credit":100,"streak_days":5,"message":"签到成功"}}`)
		case strings.HasSuffix(reqURL, "/activity/growth/heatmap"):
			score := 0
			if intlLit {
				score = 1
			}
			today := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"cells": []map[string]any{{"date": today, "score": score}}}})
			return 200, raw
		case strings.HasSuffix(reqURL, "/v2/report"):
			var evs []map[string]any
			if err := json.Unmarshal(body, &evs); err != nil {
				t.Fatalf("invalid /v2/report body: %v", err)
			}
			reportedEvents = append(reportedEvents, evs...)
			intlLit = true
			return 200, []byte(`{"code":0,"data":{}}`)
		case strings.HasSuffix(reqURL, "/billing/meter/get-user-resource-summary"):
			return 200, []byte(`{"code":0,"data":{"IsPaidUser":false,"SubscriptionPackageName":"Free","CreditGroups":[{"Kind":"plan_base","CycleTotalCapacity":"500","CycleUsedCapacity":"100","CycleRemainCapacity":"400"}]}}`)
		case strings.HasSuffix(reqURL, "/billing/meter/get-user-resource-paid-packages"):
			return 200, []byte(`{"code":0,"data":{"Accounts":[],"TotalCount":0}}`)
		case strings.HasSuffix(reqURL, "/billing/meter/get-user-resource-free-packages"):
			// Checkin / bonus package 007 not included in summary
			return 200, []byte(`{"code":0,"data":{"Accounts":[{"PackageCode":"TCACA_code_007_nzdH5h4Nl0","PackageName":"活动奖励积分","CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"100","CycleEndTime":"2099-12-31 23:59:59"}],"TotalCount":1}}`)
		default:
			return 404, []byte(`{}`)
		}
	}

	// 1. CN Check-in claims 100 credits
	cnRes, err := s.RunAccountCheckin(context.Background(), "cb-1", h.call, "workbuddy-a")
	if err != nil {
		t.Fatal(err)
	}
	if cnRes.Outcome != "claimed" || cnRes.Credit != 100 || cnRes.StreakDays != 5 || checkinCalled != 1 {
		t.Fatalf("unexpected CN checkin result: %+v (calls=%d)", cnRes, checkinCalled)
	}

	// 2. Global daily bonus probes heatmap, reports once, then detects already_lit on repeat
	intlRes, err := s.RunAccountDailyBonus(context.Background(), "cb-2", h.call, "workbuddy-ai-b")
	if err != nil {
		t.Fatal(err)
	}
	if intlRes.Outcome != "lit" || len(reportedEvents) != 1 || reportedEvents[0]["eventCode"] != "chat_request_send" {
		t.Fatalf("unexpected Global daily bonus result: %+v (events=%v)", intlRes, reportedEvents)
	}
	intlRepeat, err := s.RunAccountDailyBonus(context.Background(), "cb-3", h.call, "workbuddy-ai-b")
	if err != nil {
		t.Fatal(err)
	}
	if intlRepeat.Outcome != "already_lit" || len(reportedEvents) != 1 {
		t.Fatalf("expected already_lit without second report: %+v (events=%d)", intlRepeat, len(reportedEvents))
	}

	// 3. QuotaProvider RPC loads credential via host.auth.get and aggregates plan + 007 extra package
	quotaReq, _ := json.Marshal(map[string]any{
		"auth_index":       "workbuddy-a",
		"auth_id":          cnAuth.ID,
		"provider":         "workbuddy",
		"host_callback_id": "cb-quota",
	})
	rawQuota, err := s.Handle(context.Background(), "quota.fetch", quotaReq, h.call)
	if err != nil {
		t.Fatal(err)
	}
	quotaResp := rawQuota.(workbuddy.QuotaFetchResponse)
	if len(quotaResp.Summary) == 0 || quotaResp.Summary[0].Value != 500 {
		t.Fatalf("expected 500 remaining credits (400 plan + 100 bonus), got %+v", quotaResp.Summary)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 2 {
		t.Fatalf("expected exactly 2 quota buckets (standard + extra, no total), got %+v", quotaResp.Groups)
	}
	if quotaResp.Groups[0].Buckets[0].Window != "标准额度" || quotaResp.Groups[0].Buckets[1].Window != "额外额度" {
		t.Fatalf("unexpected bucket windows: %+v", quotaResp.Groups[0].Buckets)
	}
	if !strings.HasPrefix(quotaResp.Groups[0].Buckets[0].Description, "标准 ") || !strings.HasPrefix(quotaResp.Groups[0].Buckets[1].Description, "额外 ") {
		t.Fatalf("expected title-first bucket descriptions, got %+v", quotaResp.Groups[0].Buckets)
	}

	// 3b. Global account with plan_base group only and extra package in summary/detail must show both; without extra must show 1 bucket.
	intlQuotaReq, _ := json.Marshal(map[string]any{
		"auth_index":       "workbuddy-ai-b",
		"auth_id":          intlAuth.ID,
		"provider":         "workbuddy",
		"host_callback_id": "cb-quota-intl",
	})
	rawIntlQuota, err := s.Handle(context.Background(), "quota.fetch", intlQuotaReq, h.call)
	if err != nil {
		t.Fatal(err)
	}
	intlQuotaResp := rawIntlQuota.(workbuddy.QuotaFetchResponse)
	if len(intlQuotaResp.Groups) != 1 || len(intlQuotaResp.Groups[0].Buckets) != 2 {
		t.Fatalf("expected 2 buckets for Global account with extra package, got %+v", intlQuotaResp.Groups)
	}
	// 4. Control panel /panel and /dashboard routes
	panelRes, err := s.HandleManagement(context.Background(), "cb-panel", h.call, http.MethodGet, "/v0/resource/plugins/cpa-workbuddy/panel", url.Values{}, nil, "0.2.4")
	if err != nil || panelRes.StatusCode != 200 || !strings.Contains(string(panelRes.Body), "WorkBuddy 控制面板") {
		t.Fatalf("unexpected /panel response: status=%d err=%v", panelRes.StatusCode, err)
	}
	dashRes, err := s.HandleManagement(context.Background(), "cb-dash", h.call, http.MethodGet, "/v0/management/plugins/cpa-workbuddy/dashboard", url.Values{}, nil, "0.2.4")
	if err != nil || dashRes.StatusCode != 200 {
		t.Fatalf("unexpected /dashboard response: status=%d err=%v", dashRes.StatusCode, err)
	}
	var dash workbuddy.DashboardResponse
	if err := json.Unmarshal(dashRes.Body, &dash); err != nil || len(dash.Accounts) != 2 {
		t.Fatalf("unexpected dashboard payload: %s err=%v", dashRes.Body, err)
	}

	// 5. Panel POST /credits, /checkin, /daily-bonus and GET/PUT /settings
	creditsRes, err := s.HandleManagement(context.Background(), "cb-credits", h.call, http.MethodPost, "/v0/management/plugins/cpa-workbuddy/credits", url.Values{}, []byte(`{"auth_index":"workbuddy-a"}`), "0.2.4")
	if err != nil || creditsRes.StatusCode != 200 {
		t.Fatalf("unexpected /credits response: status=%d err=%v", creditsRes.StatusCode, err)
	}
	checkinRes, err := s.HandleManagement(context.Background(), "cb-checkin-all", h.call, http.MethodPost, "/v0/management/plugins/cpa-workbuddy/checkin", url.Values{}, []byte(`{}`), "0.2.4")
	if err != nil || checkinRes.StatusCode != 200 {
		t.Fatalf("unexpected /checkin response: status=%d err=%v", checkinRes.StatusCode, err)
	}
	bonusRes, err := s.HandleManagement(context.Background(), "cb-bonus-all", h.call, http.MethodPost, "/v0/management/plugins/cpa-workbuddy/daily-bonus", url.Values{}, []byte(`{}`), "0.2.4")
	if err != nil || bonusRes.StatusCode != 200 {
		t.Fatalf("unexpected /daily-bonus response: status=%d err=%v", bonusRes.StatusCode, err)
	}
	putSettingsRes, err := s.HandleManagement(context.Background(), "cb-put-cfg", h.call, http.MethodPut, "/v0/management/plugins/cpa-workbuddy/settings", url.Values{}, []byte(`{"cn_model_prefix":false,"intl_model_prefix":true,"auto_checkin_enabled":false,"auto_checkin_time":"10:15"}`), "0.2.4")
	if err != nil || putSettingsRes.StatusCode != 200 {
		t.Fatalf("unexpected PUT /settings response: status=%d err=%v", putSettingsRes.StatusCode, err)
	}
	getSettingsRes, err := s.HandleManagement(context.Background(), "cb-get-cfg", h.call, http.MethodGet, "/v0/management/plugins/cpa-workbuddy/settings", url.Values{}, nil, "0.2.4")
	if err != nil || getSettingsRes.StatusCode != 200 || !strings.Contains(string(getSettingsRes.Body), `"cn_model_prefix":false`) || !strings.Contains(string(getSettingsRes.Body), `"10:15"`) {
		t.Fatalf("unexpected GET /settings response: status=%d body=%s err=%v", getSettingsRes.StatusCode, getSettingsRes.Body, err)
	}
}

func TestGlobalDailyBonusNoBlindRetryOn500(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()
	intlAuth := parsedIdentity(t, "workbuddy-ai", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{intlAuth})
	reportCalls := 0
	h.handler = func(method, reqURL string, body []byte) (int, []byte) {
		if strings.HasSuffix(reqURL, "/activity/growth/heatmap") {
			return 200, []byte(`{"code":0,"data":{"cells":[{"date":"2026-11-17","score":0}]}}`)
		}
		if strings.HasSuffix(reqURL, "/v2/report") {
			reportCalls++
			return 500, []byte(`{"error":"upstream"}`)
		}
		return 404, []byte(`{}`)
	}
	res, err := s.RunAccountDailyBonus(context.Background(), "cb", h.call, "workbuddy-ai-a")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "failed" || reportCalls != 1 {
		t.Fatalf("expected single failed attempt without blind retry: res=%+v reportCalls=%d", res, reportCalls)
	}
}

func TestSchedulerJitterBoundsAndRollover(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	// Before window (04:00) with anchor 09:00: must land in [06:00, 12:00] same day.
	nowEarly := time.Date(2026, 11, 17, 4, 0, 0, 0, loc)
	for _, probe := range []int64{-workbuddy.AutoCheckinJitter, 0, workbuddy.AutoCheckinJitter} {
		next := workbuddy.ComputeNextJitteredRun(nowEarly, "09:00", "", func(minSec, maxSec int64) int64 {
			if probe < minSec {
				return minSec
			}
			if probe > maxSec {
				return maxSec
			}
			return probe
		})
		anchor := time.Date(2026, 11, 17, 9, 0, 0, 0, loc)
		diff := next.Sub(anchor)
		if diff < -3*time.Hour || diff > 3*time.Hour || !next.After(nowEarly) {
			t.Fatalf("early run out of +/-3h window: next=%v diff=%v", next, diff)
		}
	}

	// Already ran today: must roll over to tomorrow within [06:00, 12:00].
	nextTomorrow := workbuddy.ComputeNextJitteredRun(nowEarly, "09:00", "2026-11-17", func(minSec, maxSec int64) int64 { return minSec })
	wantMin := time.Date(2026, 11, 18, 6, 0, 0, 0, loc)
	if !nextTomorrow.Equal(wantMin) {
		t.Fatalf("rollover = %v, want %v", nextTomorrow, wantMin)
	}
}

func TestCheckin401RefreshesAndSavesAuthAndEnterpriseQuota(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()

	cnAuth := parsedIdentity(t, "workbuddy", "")
	entAuth := parsedIdentity(t, "workbuddy", "team")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth, entAuth})
	h.authMap["workbuddy-a"]["json"].(map[string]any)["refresh_token"] = "fixture-refresh-token"

	statusCalls := 0
	h.handler = func(method, reqURL string, body []byte) (int, []byte) {
		switch {
		case strings.HasSuffix(reqURL, "/v2/billing/meter/checkin-activity-status"):
			statusCalls++
			if statusCalls == 1 {
				return 401, []byte(`{"error":"invalid_token"}`)
			}
			return 200, []byte(`{"code":0,"data":{"active":true,"today_checked_in":false,"daily_credit":100}}`)
		case strings.HasSuffix(reqURL, "/v2/plugin/auth/token/refresh"):
			return 200, []byte(`{"code":0,"data":{"accessToken":"renewed-access","refreshToken":"renewed-refresh","expiresIn":7200,"domain":"copilot.tencent.com","tokenType":"Bearer"}}`)
		case strings.HasSuffix(reqURL, "/v3/config"):
			return 200, []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":[]}],"models":[]}}`)
		case strings.HasSuffix(reqURL, "/v2/billing/meter/daily-checkin"):
			return 200, []byte(`{"code":0,"data":{"credit":100,"streak_days":1}}`)
		case strings.HasSuffix(reqURL, "/billing/meter/get-user-resource-summary"):
			return 200, []byte(`{"code":0,"data":{"IsPaidUser":false,"Packages":[]}}`)
		case strings.HasSuffix(reqURL, "/billing/meter/get-enterprise-user-usage"):
			return 200, []byte(`{"code":0,"data":{"limitNum":1000,"credit":250,"cycleResetTime":"2026-12-01 00:00:00"}}`)
		case strings.HasSuffix(reqURL, "/billing/meter/get-user-resource-paid-packages"), strings.HasSuffix(reqURL, "/billing/meter/get-user-resource-free-packages"):
			return 200, []byte(`{"code":0,"data":{"Accounts":[],"TotalCount":0}}`)
		default:
			return 404, []byte(`{}`)
		}
	}

	res, err := s.RunAccountCheckin(context.Background(), "cb-refresh", h.call, "workbuddy-a")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "claimed" || len(h.saved) == 0 {
		t.Fatalf("expected 401 refresh + host.auth.save + claimed, got res=%+v saved=%d", res, len(h.saved))
	}

	snap, err := s.FetchAccountQuota(context.Background(), "cb-ent", h.call, "workbuddy-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Scope != "enterprise" || snap.Remaining != 750 || snap.Total != 1000 {
		t.Fatalf("unexpected enterprise quota snapshot: %+v", snap)
	}
}

func TestPanelAccountModelsRoute(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()

	cnAuth := parsedIdentity(t, "workbuddy", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth})

	configCalls := 0
	h.handler = func(method, reqURL string, body []byte) (int, []byte) {
		if strings.HasSuffix(reqURL, "/v3/config") {
			configCalls++
			return 200, []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["hy3","glm-5.3"]}],"models":[{"id":"hy3","name":"HY-3","credits":"x0.00","maxInputTokens":128000,"maxOutputTokens":32768},{"id":"glm-5.3","name":"GLM-5.3","credits":"x0.79","maxInputTokens":1000000,"maxOutputTokens":131072}]}}`)
		}
		return 404, []byte(`{}`)
	}

	if _, err := s.HandleManagement(context.Background(), "cb-bad-models", h.call, http.MethodGet, "/v0/management/plugins/cpa-workbuddy/models", url.Values{}, nil, "0.2.4"); err == nil {
		t.Fatal("expected error when auth_index is omitted")
	}

	q := url.Values{"auth_index": {"workbuddy-a"}}
	res, err := s.HandleManagement(context.Background(), "cb-models-live", h.call, http.MethodGet, "/v0/management/plugins/cpa-workbuddy/models", q, nil, "0.2.4")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("unexpected /models response: status=%d err=%v", res.StatusCode, err)
	}
	var payload workbuddy.AccountModelsResponse
	if err := json.Unmarshal(res.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != "live" || payload.Prefix != "wb/" || len(payload.Models) != 2 {
		t.Fatalf("unexpected /models payload: %+v", payload)
	}
	if payload.Models[0].Free == nil || !*payload.Models[0].Free || payload.Models[0].CreditRate == nil || *payload.Models[0].CreditRate != 0 {
		t.Fatalf("expected hy3 to be free x0.00, got %+v", payload.Models[0])
	}
	if payload.Models[1].CreditRate == nil || *payload.Models[1].CreditRate != 0.79 || (payload.Models[1].Free != nil && *payload.Models[1].Free) {
		t.Fatalf("expected glm-5.3 rate 0.79 non-free, got %+v", payload.Models[1])
	}
	if configCalls != 1 {
		t.Fatalf("expected 1 live /v3/config call, got %d", configCalls)
	}

	// Snapshot cached via host.auth.save should serve without another /v3/config call.
	res2, err := s.HandleManagement(context.Background(), "cb-models-snap", h.call, http.MethodGet, "/v0/management/plugins/cpa-workbuddy/models", q, nil, "0.2.4")
	if err != nil || res2.StatusCode != 200 {
		t.Fatalf("unexpected snapshot /models response: status=%d err=%v", res2.StatusCode, err)
	}
	var payload2 workbuddy.AccountModelsResponse
	if err := json.Unmarshal(res2.Body, &payload2); err != nil {
		t.Fatal(err)
	}
	if payload2.Source != "snapshot" || configCalls != 1 {
		t.Fatalf("expected snapshot source without extra live fetch, got source=%s calls=%d", payload2.Source, configCalls)
	}

	// refresh=1 bypasses snapshot and hits /v3/config again.
	qRefresh := url.Values{"auth_index": {"workbuddy-a"}, "refresh": {"1"}}
	res3, err := s.HandleManagement(context.Background(), "cb-models-refresh", h.call, http.MethodGet, "/v0/management/plugins/cpa-workbuddy/models", qRefresh, nil, "0.2.4")
	if err != nil || res3.StatusCode != 200 {
		t.Fatalf("unexpected refreshed /models response: status=%d err=%v", res3.StatusCode, err)
	}
	var payload3 workbuddy.AccountModelsResponse
	if err := json.Unmarshal(res3.Body, &payload3); err != nil {
		t.Fatal(err)
	}
	if payload3.Source != "live" || configCalls != 2 {
		t.Fatalf("expected live refresh call, got source=%s calls=%d", payload3.Source, configCalls)
	}
}
