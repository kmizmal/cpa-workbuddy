package workbuddy

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed assets/panel.html
var panelHTML []byte

// PanelHTML returns the embedded static control panel page bytes.
func PanelHTML() []byte {
	return panelHTML
}

// DashboardAccount summarizes one WorkBuddy credential, its quota, and today's check-in/bonus status.
type DashboardAccount struct {
	AuthIndex     string             `json:"auth_index"`
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Label         string             `json:"label"`
	Site          string             `json:"site"`
	Scope         string             `json:"scope"`
	UID           string             `json:"uid,omitempty"`
	EnterpriseID  string             `json:"enterprise_id,omitempty"`
	Status        string             `json:"status,omitempty"`
	StatusMessage string             `json:"status_message,omitempty"`
	Disabled      bool               `json:"disabled"`
	Unavailable   bool               `json:"unavailable"`
	ExpiresAt     *time.Time         `json:"expires_at,omitempty"`
	RefreshExpAt  *time.Time         `json:"refresh_expires_at,omitempty"`
	TokenExpired  bool               `json:"token_expired"`
	Quota         *QuotaSnapshot     `json:"quota,omitempty"`
	DailyStatus   *DailyActionResult `json:"daily_status,omitempty"`
	Error         string             `json:"error,omitempty"`
}

// DashboardResponse is returned by GET /v0/management/plugins/cpa-workbuddy/dashboard.
type DashboardResponse struct {
	Version               string             `json:"version"`
	Config                PluginConfig       `json:"config"`
	Fields                []ConfigField      `json:"fields"`
	Scheduler             SchedulerStatus    `json:"scheduler"`
	PrefixConflictWarning bool               `json:"prefix_conflict_warning"`
	Accounts              []DashboardAccount `json:"accounts"`
}

// AccountModelsResponse is returned by GET /v0/management/plugins/cpa-workbuddy/models.
type AccountModelsResponse struct {
	AuthIndex  string         `json:"auth_index"`
	Label      string         `json:"label"`
	Site       string         `json:"site"`
	Prefix     string         `json:"prefix"`
	CapturedAt *time.Time     `json:"captured_at,omitempty"`
	Source     string         `json:"source"`
	Models     []CatalogModel `json:"models"`
}

// ManagementResult is returned by Service.HandleManagement for plugin management/resource routes.
type ManagementResult struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// HandleManagement dispatches plugin-owned management and resource HTTP requests.
func (s *Service) HandleManagement(ctx context.Context, callbackID string, call func(string, []byte) ([]byte, error), method, path string, query url.Values, body []byte, pluginVersion string) (ManagementResult, error) {
	ctx, finish, err := s.begin(ctx, 0)
	if err != nil {
		return ManagementResult{}, err
	}
	defer finish()
	call = s.bindHostCall(call)

	const base = "/v0/management/plugins/cpa-workbuddy"
	const resBase = "/v0/resource/plugins/cpa-workbuddy"

	switch {
	case method == http.MethodGet && (path == resBase+"/panel" || path == "/panel"):
		return ManagementResult{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type":  {"text/html; charset=utf-8"},
				"Cache-Control": {"no-store"},
			},
			Body: panelHTML,
		}, nil

	case method == http.MethodGet && path == base+"/dashboard":
		refresh := query.Get("refresh") == "1" || strings.EqualFold(query.Get("refresh"), "true")
		resp, err := s.buildDashboard(ctx, callbackID, call, refresh, pluginVersion)
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, resp)

	case method == http.MethodGet && path == base+"/usage":
		refresh := query.Get("refresh") == "1" || strings.EqualFold(query.Get("refresh"), "true")
		resp, err := s.buildUsageStatistics(ctx, callbackID, call, refresh)
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, resp)

	case method == http.MethodPost && path == base+"/usage/reset":
		s.usage.reset()
		return jsonManagementResult(http.StatusOK, map[string]any{"ok": true})

	case method == http.MethodGet && path == base+"/models":
		authIndex := strings.TrimSpace(query.Get("auth_index"))
		refresh := query.Get("refresh") == "1" || strings.EqualFold(query.Get("refresh"), "true")
		resp, err := s.fetchAccountModels(ctx, callbackID, call, authIndex, refresh)
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, resp)

	case method == http.MethodPost && path == base+"/transfer/preview":
		var req struct {
			Text string `json:"text"`
		}
		if err := decode(body, &req); err != nil {
			return ManagementResult{}, err
		}
		preview, err := previewTransferFile([]byte(req.Text))
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, preview)

	case method == http.MethodPost && path == base+"/transfer/export":
		var req struct {
			AuthIndexes []string `json:"auth_indexes"`
		}
		if err := decode(body, &req); err != nil {
			return ManagementResult{}, err
		}
		if len(req.AuthIndexes) == 0 {
			return ManagementResult{}, failure("invalid_transfer", "auth_indexes must contain at least one account", 400)
		}
		wanted := make(map[string]bool, len(req.AuthIndexes))
		for _, index := range req.AuthIndexes {
			if strings.TrimSpace(index) != "" {
				wanted[index] = true
			}
		}
		entries, err := hostAuthList(call)
		if err != nil {
			return ManagementResult{}, err
		}
		records := make([]transferRecord, 0, len(wanted))
		for _, entry := range entries {
			if !wanted[entry.AuthIndex] {
				continue
			}
			rec, getErr := hostAuthGet(call, entry.AuthIndex)
			if getErr != nil {
				return ManagementResult{}, getErr
			}
			out, outErr := transferRecordOf(rec)
			if outErr != nil {
				return ManagementResult{}, outErr
			}
			records = append(records, out)
		}
		if len(records) == 0 {
			return ManagementResult{}, failure("auth_not_found", "none of the selected accounts was found", 409)
		}
		return jsonManagementResult(http.StatusOK, map[string]any{"accounts": records})

	case method == http.MethodPost && path == base+"/transfer/import":
		var req struct {
			Text    string `json:"text"`
			Indexes []int  `json:"indexes"`
		}
		if err := decode(body, &req); err != nil {
			return ManagementResult{}, err
		}
		result, err := importTransferRecords(call, req.Text, req.Indexes)
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, result)

	case method == http.MethodPost && path == base+"/credits":
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := decode(body, &req); err != nil {
				return ManagementResult{}, err
			}
		}
		accounts, err := s.refreshCredits(ctx, callbackID, call, strings.TrimSpace(req.AuthIndex))
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, map[string]any{"accounts": accounts})

	case method == http.MethodPost && path == base+"/checkin":
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := decode(body, &req); err != nil {
				return ManagementResult{}, err
			}
		}
		if idx := strings.TrimSpace(req.AuthIndex); idx != "" {
			res, err := s.RunAccountCheckin(ctx, callbackID, call, idx)
			if err != nil {
				return ManagementResult{}, err
			}
			return jsonManagementResult(http.StatusOK, map[string]any{"results": []DailyActionResult{res}})
		}
		results, err := s.RunAllDailyActions(ctx, callbackID, call, "checkin")
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, map[string]any{"results": results})

	case method == http.MethodPost && path == base+"/daily-bonus":
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := decode(body, &req); err != nil {
				return ManagementResult{}, err
			}
		}
		if idx := strings.TrimSpace(req.AuthIndex); idx != "" {
			res, err := s.RunAccountDailyBonus(ctx, callbackID, call, idx)
			if err != nil {
				return ManagementResult{}, err
			}
			return jsonManagementResult(http.StatusOK, map[string]any{"results": []DailyActionResult{res}})
		}
		results, err := s.RunAllDailyActions(ctx, callbackID, call, "daily_bonus")
		if err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, map[string]any{"results": results})

	case method == http.MethodGet && path == base+"/settings":
		return jsonManagementResult(http.StatusOK, map[string]any{
			"config":    s.Config(),
			"fields":    ConfigFields(),
			"scheduler": s.SchedulerSnapshot(),
		})

	case method == http.MethodPut && path == base+"/settings":
		var nextCfg PluginConfig
		if err := decode(body, &nextCfg); err != nil {
			return ManagementResult{}, err
		}
		if err := s.UpdateConfig(nextCfg); err != nil {
			return ManagementResult{}, err
		}
		return jsonManagementResult(http.StatusOK, map[string]any{
			"config":    s.Config(),
			"scheduler": s.SchedulerSnapshot(),
		})

	default:
		return ManagementResult{}, failure("route_not_found", "unknown management path", 404)
	}
}

func jsonManagementResult(status int, payload any) (ManagementResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return ManagementResult{}, failure("internal_error", err.Error(), 500)
	}
	return ManagementResult{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: body,
	}, nil
}

func (s *Service) buildDashboard(ctx context.Context, callbackID string, call func(string, []byte) ([]byte, error), refresh bool, pluginVersion string) (DashboardResponse, error) {
	entries, err := hostAuthList(call)
	if err != nil {
		return DashboardResponse{}, err
	}

	accounts := make([]DashboardAccount, len(entries))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	now := time.Now().UTC()

	for i, entry := range entries {
		wg.Add(1)
		go func(idx int, e hostAuthEntry) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			item := DashboardAccount{
				AuthIndex:     e.AuthIndex,
				ID:            e.ID,
				Name:          e.Name,
				Label:         e.Label,
				Status:        e.Status,
				StatusMessage: e.StatusMessage,
				Disabled:      e.Disabled,
				Unavailable:   e.Unavailable,
			}

			rec, rErr := hostAuthGet(call, e.AuthIndex)
			if rErr == nil {
				item.Site = rec.Site.id
				item.UID = rec.Stored.UID
				item.EnterpriseID = rec.Stored.EnterpriseID
				if rec.Stored.EnterpriseID != "" {
					item.Scope = "enterprise"
				} else {
					item.Scope = "personal"
				}
				if item.Label == "" {
					item.Label = rec.Auth.Label
				}
				if !rec.Stored.ExpiresAt.IsZero() {
					exp := rec.Stored.ExpiresAt.UTC()
					item.ExpiresAt = &exp
					item.TokenExpired = !exp.After(now)
				}
				if rec.Stored.RefreshExpiresAt != nil {
					rexp := rec.Stored.RefreshExpiresAt.UTC()
					item.RefreshExpAt = &rexp
				}
			} else {
				item.Error = rErr.Error()
			}

			if refresh && !e.Disabled && rErr == nil {
				if freshRec, fErr := s.ensureFreshCredential(ctx, callbackID, call, rec); fErr == nil {
					rec = freshRec
				}
				mu := s.accountLock(rec.AuthIndex)
				mu.Lock()
				snap, nextRec, qErr := s.fetchQuotaWithRefresh(ctx, callbackID, call, rec)
				if qErr == nil {
					s.cacheQuota(nextRec.AuthIndex, snap)
					item.Quota = &snap
					rec = nextRec
				} else {
					item.Error = qErr.Error()
				}
				if daily, _, dErr := s.ProbeAccountDailyStatus(ctx, callbackID, call, rec); dErr == nil {
					item.DailyStatus = &daily
				}
				mu.Unlock()
			} else {
				cached := s.cachedBilling(e.AuthIndex)
				item.Quota = cached.Quota
				item.DailyStatus = cached.DailyStatus
			}

			accounts[idx] = item
		}(i, entry)
	}
	wg.Wait()

	cfg := s.Config()
	return DashboardResponse{
		Version:               pluginVersion,
		Config:                cfg,
		Fields:                ConfigFields(),
		Scheduler:             s.SchedulerSnapshot(),
		PrefixConflictWarning: !cfg.CNModelPrefix && !cfg.IntlModelPrefix,
		Accounts:              accounts,
	}, nil
}

func (s *Service) refreshCredits(ctx context.Context, callbackID string, call func(string, []byte) ([]byte, error), authIndex string) ([]DashboardAccount, error) {
	if authIndex != "" {
		snap, err := s.FetchAccountQuota(ctx, callbackID, call, authIndex, nil)
		if err != nil {
			return nil, err
		}
		rec, rErr := s.resolveAuthRecord(ctx, callbackID, call, authIndex, nil)
		var daily *DailyActionResult
		if rErr == nil {
			mu := s.accountLock(rec.AuthIndex)
			mu.Lock()
			if d, _, dErr := s.ProbeAccountDailyStatus(ctx, callbackID, call, rec); dErr == nil {
				daily = &d
			}
			mu.Unlock()
		}
		return []DashboardAccount{{
			AuthIndex:   authIndex,
			Site:        snap.Site,
			Scope:       snap.Scope,
			Quota:       &snap,
			DailyStatus: daily,
		}}, nil
	}

	dash, err := s.buildDashboard(ctx, callbackID, call, true, "")
	if err != nil {
		return nil, err
	}
	return dash.Accounts, nil
}

func (s *Service) fetchAccountModels(ctx context.Context, callbackID string, call func(string, []byte) ([]byte, error), authIndex string, refresh bool) (AccountModelsResponse, error) {
	if authIndex == "" {
		return AccountModelsResponse{}, failure("invalid_request", "auth_index is required", 400)
	}
	rec, err := s.resolveAuthRecord(ctx, callbackID, call, authIndex, nil)
	if err != nil {
		return AccountModelsResponse{}, err
	}
	prefix := ""
	if s.modelPrefixEnabled(rec.Site.id) {
		prefix = rec.Site.modelPrefix
	}
	label := strings.TrimSpace(rec.Auth.Label)
	if label == "" {
		label = strings.TrimSpace(rec.FileName)
	}
	if label == "" {
		label = rec.Auth.ID
	}
	if !refresh && validModelSnapshot(rec.Stored.ModelSnapshot, rec.Auth.ID, rec.Site.id) {
		capturedAt := rec.Stored.ModelSnapshot.CapturedAt.UTC()
		models := filterCatalogModels(rec.Site.id, rec.Stored.ModelSnapshot.Models)
		return AccountModelsResponse{
			AuthIndex:  rec.AuthIndex,
			Label:      label,
			Site:       rec.Site.id,
			Prefix:     prefix,
			CapturedAt: &capturedAt,
			Source:     "snapshot",
			Models:     models,
		}, nil
	}

	mu := s.accountLock(rec.AuthIndex)
	mu.Lock()
	defer mu.Unlock()

	fetchConfig := func(r hostAuthRecord) (json.RawMessage, error) {
		headers, hErr := r.Auth.RequestHeaders()
		if hErr != nil {
			return nil, hErr
		}
		var cfg json.RawMessage
		if reqErr := request(ctx, r.Site, callbackID, call, http.MethodGet, "/v3/config", headers, nil, &cfg); reqErr != nil {
			return nil, reqErr
		}
		return cfg, nil
	}

	rawConfig, err := fetchConfig(rec)
	if err != nil && isAPIUnauthorized(err) {
		if refreshed, rErr := s.refreshAndSaveRecord(ctx, callbackID, call, rec); rErr == nil {
			rec = refreshed
			rawConfig, err = fetchConfig(rec)
		}
	}
	if err != nil {
		return AccountModelsResponse{}, toPublicError(err)
	}

	now := time.Now().UTC()
	models, err := ParseModelConfig(rec.Site.id, rawConfig, now)
	if err != nil {
		return AccountModelsResponse{}, err
	}
	if models == nil {
		models = []CatalogModel{}
	}
	rec.Stored.ModelSnapshot = &modelSnapshot{
		AuthID:     rec.Auth.ID,
		Site:       rec.Site.id,
		CapturedAt: now,
		Models:     models,
	}
	rec.Stored.ModelDiscoveryFailure = ""
	if nextAuth, aErr := authFromCredential(rec.Stored, rec.Auth.Metadata); aErr == nil {
		if saveErr := hostAuthSaveCredential(call, rec, nextAuth); saveErr != nil {
			emitHostLog(call, callbackID, "warn", "WorkBuddy failed to persist model snapshot", map[string]any{
				"auth_index": rec.AuthIndex,
				"error":      saveErr.Error(),
			})
		}
	}
	return AccountModelsResponse{
		AuthIndex:  rec.AuthIndex,
		Label:      label,
		Site:       rec.Site.id,
		Prefix:     prefix,
		CapturedAt: &now,
		Source:     "live",
		Models:     models,
	}, nil
}
