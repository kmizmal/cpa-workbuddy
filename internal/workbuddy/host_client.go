package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"strings"
	"time"
)

const billingRefreshLead = 5 * time.Minute

type hostAuthEntry struct {
	ID            string `json:"id,omitempty"`
	AuthIndex     string `json:"auth_index,omitempty"`
	Name          string `json:"name"`
	Type          string `json:"type,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Label         string `json:"label,omitempty"`
	Status        string `json:"status,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	Unavailable   bool   `json:"unavailable,omitempty"`
}

type hostAuthRecord struct {
	AuthIndex string
	FileName  string
	Path      string
	RawJSON   []byte
	Auth      AuthData
	Stored    credential
	Site      site
}

func (s *Service) bindHostCall(call func(string, []byte) ([]byte, error)) func(string, []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if call != nil {
		s.hostCall = call
		return call
	}
	return s.hostCall
}

func (s *Service) currentHostCall() func(string, []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostCall
}

func hostAuthList(call func(string, []byte) ([]byte, error)) ([]hostAuthEntry, error) {
	if call == nil {
		return nil, failure("host_unavailable", "host auth callback is unavailable", 503)
	}
	reply, err := call("host.auth.list", []byte("{}"))
	if err != nil {
		return nil, failure("host_auth_unavailable", "cannot list credentials from the CPA host", 503)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Files []hostAuthEntry `json:"files"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil || !envelope.OK {
		msg := "cannot list credentials from the CPA host"
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			msg = envelope.Error.Message
		}
		return nil, failure("host_auth_unavailable", msg, 503)
	}
	out := make([]hostAuthEntry, 0, len(envelope.Result.Files))
	for _, entry := range envelope.Result.Files {
		if entry.Provider == provider || entry.Type == provider {
			out = append(out, entry)
		}
	}
	return out, nil
}

func hostAuthGet(call func(string, []byte) ([]byte, error), authIndex string) (hostAuthRecord, error) {
	if call == nil {
		return hostAuthRecord{}, failure("host_unavailable", "host auth callback is unavailable", 503)
	}
	reqRaw, err := json.Marshal(struct {
		AuthIndex string `json:"auth_index"`
	}{AuthIndex: authIndex})
	if err != nil {
		return hostAuthRecord{}, failure("internal_error", "cannot encode host auth lookup", 500)
	}
	reply, err := call("host.auth.get", reqRaw)
	if err != nil {
		return hostAuthRecord{}, failure("host_auth_unavailable", "cannot load credential from the CPA host", 503)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			AuthIndex string          `json:"auth_index"`
			Name      string          `json:"name"`
			Path      string          `json:"path"`
			JSON      json.RawMessage `json:"json"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil || !envelope.OK {
		msg := "cannot load credential from the CPA host"
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			msg = envelope.Error.Message
		}
		return hostAuthRecord{}, failure("auth_not_found", msg, 404)
	}
	return parseHostAuthRecord(envelope.Result.AuthIndex, envelope.Result.Name, envelope.Result.Path, envelope.Result.JSON)
}

func parseHostAuthRecord(authIndex, name, path string, rawJSON []byte) (hostAuthRecord, error) {
	stored, metadata, handled, err := credentialFromJSON(rawJSON)
	if err != nil {
		return hostAuthRecord{}, err
	}
	if !handled {
		return hostAuthRecord{}, failure("invalid_credentials", "credential is not a supported WorkBuddy browser auth", 400)
	}
	chosen, err := credentialSite(stored)
	if err != nil {
		return hostAuthRecord{}, err
	}
	authData, err := authFromCredential(stored, metadata)
	if err != nil {
		return hostAuthRecord{}, err
	}
	fileName := strings.TrimSpace(name)
	if fileName == "" && strings.TrimSpace(path) != "" {
		fileName = filepath.Base(path)
	}
	if fileName == "" {
		fileName = stored.FileName
	}
	if !strings.HasSuffix(strings.ToLower(fileName), ".json") && fileName != "" {
		fileName += ".json"
	}
	return hostAuthRecord{
		AuthIndex: authIndex,
		FileName:  fileName,
		Path:      path,
		RawJSON:   bytes.Clone(rawJSON),
		Auth:      authData,
		Stored:    stored,
		Site:      chosen,
	}, nil
}

func hostAuthSaveNewCredential(call func(string, []byte) ([]byte, error), auth AuthData) error {
	if call == nil || strings.TrimSpace(auth.FileName) == "" {
		return failure("host_unavailable", "host auth callback is unavailable", 503)
	}
	saveReq, err := json.Marshal(struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	}{Name: filepath.Base(auth.FileName), JSON: auth.StorageJSON})
	if err != nil {
		return err
	}
	reply, err := call("host.auth.save", saveReq)
	if err != nil {
		return err
	}
	var envelope struct {
		OK    bool `json:"ok"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil || !envelope.OK {
		return failure("host_save_failed", "host failed to save imported credential", 502)
	}
	return nil
}

func hostAuthSaveCredential(call func(string, []byte) ([]byte, error), rec hostAuthRecord, nextAuth AuthData) error {
	if call == nil || strings.TrimSpace(rec.FileName) == "" {
		return nil
	}
	baseMap := make(map[string]any)
	if len(bytes.TrimSpace(rec.RawJSON)) > 0 {
		if err := json.Unmarshal(rec.RawJSON, &baseMap); err != nil {
			return err
		}
	}
	var updatedMap map[string]any
	if err := json.Unmarshal(nextAuth.StorageJSON, &updatedMap); err != nil {
		return err
	}
	maps.Copy(baseMap, updatedMap)
	mergedRaw, err := json.Marshal(baseMap)
	if err != nil {
		return err
	}
	saveReq, err := json.Marshal(struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	}{
		Name: filepath.Base(rec.FileName),
		JSON: mergedRaw,
	})
	if err != nil {
		return err
	}
	reply, err := call("host.auth.save", saveReq)
	if err != nil {
		return err
	}
	var envelope struct {
		OK    bool `json:"ok"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil || !envelope.OK {
		msg := "host failed to save refreshed credential"
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			msg = envelope.Error.Message
		}
		return failure("host_save_failed", msg, 502)
	}
	return nil
}

func (s *Service) resolveAuthRecord(ctx context.Context, callbackID string, call func(string, []byte) ([]byte, error), authIndex string, inlineStorage []byte) (hostAuthRecord, error) {
	if len(bytes.TrimSpace(inlineStorage)) > 0 {
		rec, err := parseHostAuthRecord(authIndex, "", "", inlineStorage)
		if err == nil {
			return s.ensureFreshCredential(ctx, callbackID, call, rec)
		}
	}
	rec, err := hostAuthGet(call, authIndex)
	if err != nil {
		return hostAuthRecord{}, err
	}
	return s.ensureFreshCredential(ctx, callbackID, call, rec)
}

func (s *Service) ensureFreshCredential(ctx context.Context, callbackID string, call func(string, []byte) ([]byte, error), rec hostAuthRecord) (hostAuthRecord, error) {
	if rec.Stored.ExpiresAt.IsZero() || time.Now().Add(billingRefreshLead).Before(rec.Stored.ExpiresAt) {
		return rec, nil
	}
	if strings.TrimSpace(rec.Stored.RefreshToken) == "" {
		return rec, nil
	}
	refreshed, err := s.refreshAndSaveRecord(ctx, callbackID, call, rec)
	if err != nil {
		// If access token is still valid (within the 5m lead), allow the call to
		// attempt upstream before declaring failure.
		if time.Now().Before(rec.Stored.ExpiresAt) {
			return rec, nil
		}
		return hostAuthRecord{}, err
	}
	return refreshed, nil
}

func (s *Service) refreshAndSaveRecord(ctx context.Context, callbackID string, call func(string, []byte) ([]byte, error), rec hostAuthRecord) (hostAuthRecord, error) {
	nextAuth, err := s.renew(ctx, rec.Auth, callbackID, call)
	if err != nil {
		return hostAuthRecord{}, err
	}
	nextStored, _, _, err := credentialFromJSON(nextAuth.StorageJSON)
	if err != nil {
		return hostAuthRecord{}, err
	}
	if err := hostAuthSaveCredential(call, rec, nextAuth); err != nil {
		emitHostLog(call, callbackID, "warn", "WorkBuddy failed to persist refreshed credential", map[string]any{
			"auth_index": rec.AuthIndex,
			"auth_id":    rec.Auth.ID,
			"error":      err.Error(),
		})
	}
	rec.Auth = nextAuth
	rec.Stored = nextStored
	return rec, nil
}

func emitHostLog(call func(string, []byte) ([]byte, error), callbackID, level, message string, fields map[string]any) {
	if call == nil {
		return
	}
	raw, err := json.Marshal(struct {
		HostCallbackID string         `json:"host_callback_id,omitempty"`
		Level          string         `json:"level"`
		Message        string         `json:"message"`
		Fields         map[string]any `json:"fields,omitempty"`
	}{
		HostCallbackID: callbackID,
		Level:          level,
		Message:        message,
		Fields:         fields,
	})
	if err != nil {
		return
	}
	_, _ = call("host.log", raw)
}
