package workbuddy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cpa-workbuddy/internal/workbuddy"
)

var errHostGetFailed = errors.New("host auth get failed")

const transferPreviewPath = "/v0/management/plugins/cpa-workbuddy/transfer/preview"
const transferExportPath = "/v0/management/plugins/cpa-workbuddy/transfer/export"
const transferImportPath = "/v0/management/plugins/cpa-workbuddy/transfer/import"

func postManagement(t *testing.T, s *workbuddy.Service, h *fakeHostHarness, path, body string) (workbuddy.ManagementResult, error) {
	t.Helper()
	return s.HandleManagement(context.Background(), "cb-transfer", h.call, http.MethodPost, path, url.Values{}, []byte(body), "0.2.4")
}

func TestTransferRoutesPreviewExportImport(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()

	cnAuth := parsedIdentity(t, "workbuddy", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth})

	// 1. preview: redacted, reports importability, never echoes the token.
	const token = "route-preview-secret"
	previewRes, err := postManagement(t, s, h, transferPreviewPath,
		`{"text":"{\"accounts\":[{\"access_token\":\"`+token+`\",\"uid\":\"uid-route\",\"nickname\":\"路线账号\",\"domain\":\"copilot.tencent.com\"}]}"}`)
	if err != nil || previewRes.StatusCode != 200 {
		t.Fatalf("unexpected /transfer/preview response: status=%d err=%v", previewRes.StatusCode, err)
	}
	if strings.Contains(string(previewRes.Body), token) {
		t.Fatalf("preview leaked the access token: %s", previewRes.Body)
	}
	var preview struct {
		Accounts []struct {
			Index      int    `json:"index"`
			Nickname   string `json:"nickname"`
			Site       string `json:"site"`
			HasToken   bool   `json:"has_token"`
			Importable bool   `json:"importable"`
		} `json:"accounts"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(previewRes.Body, &preview); err != nil {
		t.Fatalf("invalid preview payload: %s err=%v", previewRes.Body, err)
	}
	if preview.Total != 1 || len(preview.Accounts) != 1 || !preview.Accounts[0].Importable ||
		!preview.Accounts[0].HasToken || preview.Accounts[0].Site != "workbuddy" {
		t.Fatalf("unexpected preview payload: %s", previewRes.Body)
	}

	// 2. import: reports duplicated separately and stores exactly one credential.
	importRes, err := postManagement(t, s, h, transferImportPath,
		`{"text":"[{\"access_token\":\"first\",\"uid\":\"uid-dup\",\"domain\":\"copilot.tencent.com\"},{\"access_token\":\"second\",\"uid\":\"uid-dup\",\"domain\":\"copilot.tencent.com\"}]","indexes":[0,1]}`)
	if err != nil || importRes.StatusCode != 200 {
		t.Fatalf("unexpected /transfer/import response: status=%d err=%v", importRes.StatusCode, err)
	}
	var imported struct {
		Imported   int `json:"imported"`
		Skipped    int `json:"skipped"`
		Duplicated int `json:"duplicated"`
	}
	if err := json.Unmarshal(importRes.Body, &imported); err != nil {
		t.Fatalf("invalid import payload: %s err=%v", importRes.Body, err)
	}
	if imported.Imported != 1 || imported.Skipped != 0 || imported.Duplicated != 1 {
		t.Fatalf("unexpected import counts: %s", importRes.Body)
	}

	// 3. export: the stored credential round-trips through the reference envelope.
	exportRes, err := postManagement(t, s, h, transferExportPath, `{"auth_indexes":["workbuddy-a"]}`)
	if err != nil || exportRes.StatusCode != 200 {
		t.Fatalf("unexpected /transfer/export response: status=%d err=%v", exportRes.StatusCode, err)
	}
	var exported struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(exportRes.Body, &exported); err != nil {
		t.Fatalf("invalid export payload: %s err=%v", exportRes.Body, err)
	}
	if len(exported.Accounts) != 1 {
		t.Fatalf("expected one exported account: %s", exportRes.Body)
	}
	if exported.Accounts[0]["auth_raw"] == nil {
		t.Fatalf("export lost auth_raw: %s", exportRes.Body)
	}
	if _, ok := exported.Accounts[0]["expiresAt"]; !ok {
		t.Fatalf("export lost expiresAt: %s", exportRes.Body)
	}

	// 4. Round trip: feed the exported file back through the REAL import path
	// and compare the stored credential. Preview alone would not catch a domain
	// rewritten into a URL or an expiry the host silently rejects.
	reimport := map[string]any{"accounts": exported.Accounts}
	rawReimport, err := json.Marshal(reimport)
	if err != nil {
		t.Fatal(err)
	}
	before := storedCredential(t, h, "workbuddy-a")
	reimportRes, err := postManagement(t, s, h, transferImportPath,
		`{"text":`+string(mustJSONQuote(t, string(rawReimport)))+`,"indexes":[0]}`)
	if err != nil || reimportRes.StatusCode != 200 {
		t.Fatalf("exported file was rejected by import: status=%d err=%v body=%s", reimportRes.StatusCode, err, reimportRes.Body)
	}
	var again struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal(reimportRes.Body, &again); err != nil {
		t.Fatalf("invalid re-import payload: %s err=%v", reimportRes.Body, err)
	}
	if again.Imported != 1 || again.Skipped != 0 {
		t.Fatalf("exported file did not re-import cleanly: %s", reimportRes.Body)
	}
	after := storedCredential(t, h, "workbuddy-a")
	// The fixture credential carries no domain, so export fills it from the
	// site. What matters is that the value is a bare hostname the upstream
	// request path can put in X-Domain, not a URL.
	if after.Domain != "copilot.tencent.com" {
		t.Fatalf("stored domain is not the bare site hostname: %q (was %q)", after.Domain, before.Domain)
	}
	if after.AccessToken != before.AccessToken {
		t.Fatal("round trip changed the access token")
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("round trip changed the expiry: %v -> %v", before.ExpiresAt, after.ExpiresAt)
	}
}

// storedCredential reads the credential the fake host currently holds for an
// auth index, decoding the WorkBuddy browser document it stores.
type storedAccount struct {
	AccessToken string    `json:"accessToken"`
	Domain      string    `json:"domain"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

func storedCredential(t *testing.T, h *fakeHostHarness, authIndex string) storedAccount {
	t.Helper()
	request, err := json.Marshal(map[string]any{"auth_index": authIndex})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := h.call("host.auth.get", request)
	if err != nil {
		t.Fatalf("host.auth.get failed: %v", err)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Auth struct {
			StorageJSON json.RawMessage `json:"json"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("invalid host.auth.get payload: %s err=%v", raw, err)
	}
	if !envelope.OK {
		t.Fatalf("host.auth.get reported failure: %s", raw)
	}
	var stored storedAccount
	if err := json.Unmarshal(envelope.Auth.StorageJSON, &stored); err != nil {
		t.Fatalf("invalid stored credential: %s err=%v", envelope.Auth.StorageJSON, err)
	}
	return stored
}

// Importing over an existing account must only refresh token fields. The
// host-managed cache, discovery failure state, display name and file name are
// owned by the existing credential and must survive a transfer that omits them.
func TestTransferImportPreservesExistingCredentialState(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()
	cnAuth := parsedIdentity(t, "workbuddy", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth})

	const existingName = "existing-account.json"
	h.mu.Lock()
	raw := h.authMap["workbuddy-a"]["json"].(map[string]any)
	// The previous importer could leave a stale embedded file_name. The host
	// save name stays authoritative when a later import repairs the record.
	raw["file_name"] = "stale-account.json"
	raw["refresh_token"] = "old-refresh"
	raw["refresh_expires_at"] = "2031-01-01T00:00:00Z"
	raw["display_name"] = "Existing Name"
	raw["model_snapshot"] = map[string]any{
		"captured_at": "2026-01-01T00:00:00Z",
		"site":        "test",
		"models":      []any{},
	}
	raw["model_discovery_failure"] = "old discovery failure"
	h.authMap["workbuddy-a"]["name"] = existingName
	h.authMap["workbuddy-a"]["path"] = "/tmp/" + existingName
	h.files[0]["name"] = existingName
	h.mu.Unlock()

	res, err := postManagement(t, s, h, transferImportPath,
		`{"text":"[{\"access_token\":\"new-access\",\"uid\":\"person\",\"domain\":\"copilot.tencent.com\"}]","indexes":[0]}`)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("unexpected import response: status=%d err=%v body=%s", res.StatusCode, err, res.Body)
	}

	h.mu.Lock()
	after := h.authMap["workbuddy-a"]["json"].(map[string]any)
	h.mu.Unlock()
	if after["file_name"] != existingName {
		t.Fatalf("file_name changed: got %v want %q", after["file_name"], existingName)
	}
	if after["refresh_token"] != "old-refresh" {
		t.Fatalf("refresh_token was cleared: %v", after["refresh_token"])
	}
	if after["refresh_expires_at"] != "2031-01-01T00:00:00Z" {
		t.Fatalf("refresh_expires_at was cleared: %v", after["refresh_expires_at"])
	}
	if after["display_name"] != "Existing Name" {
		t.Fatalf("display_name was cleared: %v", after["display_name"])
	}
	if after["model_discovery_failure"] != "old discovery failure" {
		t.Fatalf("model_discovery_failure was cleared: %v", after["model_discovery_failure"])
	}
	snapshot, ok := after["model_snapshot"].(map[string]any)
	if !ok || snapshot["captured_at"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("model_snapshot was lost: %v", after["model_snapshot"])
	}

	// A non-empty display name from the import is allowed to replace the
	// existing one; the empty case above must not.
	res, err = postManagement(t, s, h, transferImportPath,
		`{"text":"[{\"access_token\":\"newer-access\",\"uid\":\"person\",\"nickname\":\"New Name\",\"domain\":\"copilot.tencent.com\"}]","indexes":[0]}`)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("unexpected second import response: status=%d err=%v body=%s", res.StatusCode, err, res.Body)
	}
	h.mu.Lock()
	after = h.authMap["workbuddy-a"]["json"].(map[string]any)
	h.mu.Unlock()
	if after["display_name"] != "New Name" {
		t.Fatalf("non-empty nickname did not override display_name: %v", after["display_name"])
	}
}

func TestTransferImportIsolatesBadRecordsAndHostReadFailures(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()

	cnAuth := parsedIdentity(t, "workbuddy", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth})
	// One valid record, one entry that is not an object, one with an unknown
	// domain: the batch must import the valid one and skip the other two.
	res, err := postManagement(t, s, h, transferImportPath,
		`{"text":"[{\"access_token\":\"good\",\"uid\":\"uid-good\",\"domain\":\"copilot.tencent.com\"},\"not-an-object\",{\"access_token\":\"x\",\"uid\":\"uid-bad\",\"domain\":\"unknown.example\"}]","indexes":[0,1,2]}`)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("unexpected import response: status=%d err=%v", res.StatusCode, err)
	}
	var counts struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal(res.Body, &counts); err != nil {
		t.Fatalf("invalid payload: %s err=%v", res.Body, err)
	}
	if counts.Imported != 1 || counts.Skipped != 2 {
		t.Fatalf("bad records were not isolated: %s", res.Body)
	}

	// A failing host read must surface as an error, not be silently skipped.
	failing := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth})
	failing.callOverride = func(method string) ([]byte, error, bool) {
		if method == "host.auth.get" {
			return nil, errHostGetFailed, true
		}
		return nil, nil, false
	}
	_, err = postManagement(t, s, failing, transferImportPath,
		`{"text":"[{\"access_token\":\"new\",\"uid\":\"uid-new\",\"domain\":\"copilot.tencent.com\"}]","indexes":[0]}`)
	if err == nil {
		t.Fatal("expected host read failure to surface as an error")
	}
}

// A host save failure must name the host's own reason and report how many
// credentials were stored before it, so the client can tell whether a retry
// would duplicate accounts.
func TestTransferImportReportsStoredCountAndHostReasonOnSaveFailure(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()
	cnAuth := parsedIdentity(t, "workbuddy", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth})
	saveCalls := 0
	h.callOverride = func(method string) ([]byte, error, bool) {
		if method == "host.auth.save" {
			saveCalls++
			if saveCalls > 1 {
				return []byte(`{"ok":false,"error":{"message":"auth file name must end with .json"}}`), nil, true
			}
		}
		return nil, nil, false
	}

	_, err := postManagement(t, s, h, transferImportPath,
		`{"text":"[{\"access_token\":\"new-1\",\"uid\":\"uid-new-1\",\"domain\":\"copilot.tencent.com\"},{\"access_token\":\"new-2\",\"uid\":\"uid-new-2\",\"domain\":\"copilot.tencent.com\"}]","indexes":[0,1]}`)
	if err == nil {
		t.Fatal("expected the host save failure to surface")
	}
	if !strings.Contains(err.Error(), "auth file name must end with .json") {
		t.Fatalf("host reason was swallowed: %v", err)
	}
	if !strings.Contains(err.Error(), "imported 1") {
		t.Fatalf("partial written count was lost: %v", err)
	}
}

func TestTransferImportRejectsExistingCredentialWithoutHostFilename(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()
	h := newFakeHostHarness(t, []workbuddy.AuthData{parsedIdentity(t, "workbuddy", "")})
	h.mu.Lock()
	delete(h.authMap["workbuddy-a"], "name")
	delete(h.authMap["workbuddy-a"], "path")
	delete(h.authMap["workbuddy-a"]["json"].(map[string]any), "file_name")
	h.mu.Unlock()

	_, err := postManagement(t, s, h, transferImportPath,
		`{"text":"[{\"access_token\":\"new-access\",\"uid\":\"person\",\"domain\":\"copilot.tencent.com\"}]","indexes":[0]}`)
	if err == nil || !strings.Contains(err.Error(), "imported 0 credential(s)") || !strings.Contains(err.Error(), "missing the host file name") {
		t.Fatalf("expected a zero-write error for an unnamed credential, got %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.saved) != 0 {
		t.Fatalf("unnamed credential was saved: %d files", len(h.saved))
	}
}

func TestTransferExportRejectsUnknownSelection(t *testing.T) {
	s := workbuddy.New()
	defer s.Close()
	cnAuth := parsedIdentity(t, "workbuddy", "")
	h := newFakeHostHarness(t, []workbuddy.AuthData{cnAuth})

	res, err := postManagement(t, s, h, transferExportPath, `{"auth_indexes":["missing"]}`)
	if err == nil && res.StatusCode == 200 {
		t.Fatalf("expected an error for an unknown auth_index, got %d", res.StatusCode)
	}
	if err == nil {
		t.Fatalf("expected a protocol error, got body %s", res.Body)
	}

	res, err = postManagement(t, s, h, transferExportPath, `{"auth_indexes":[]}`)
	if err == nil && res.StatusCode == 200 {
		t.Fatal("expected an error for an empty selection")
	}
}

func mustJSONQuote(t *testing.T, raw string) string {
	t.Helper()
	quoted, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(quoted)
}
