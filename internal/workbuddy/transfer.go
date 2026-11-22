package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// transferRecord is the JSON contract shared with dsh-connect-workbuddy.
type transferRecord struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	UID              string `json:"uid,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
	Email            string `json:"email,omitempty"`
	Domain           string `json:"domain,omitempty"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt,omitempty"`
	AuthRaw          any    `json:"auth_raw,omitempty"`
}

type transferPreviewEntry struct {
	Index    int    `json:"index"`
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Email    string `json:"email,omitempty"`
	HasToken bool   `json:"has_token"`
}

type transferPreview struct {
	Accounts []transferPreviewEntry `json:"accounts"`
	Total    int                    `json:"total"`
}

func parseTransferFile(raw []byte) ([]transferRecord, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, failure("invalid_transfer", "transfer file is empty", 400)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, failure("invalid_transfer", "transfer file is not valid JSON", 400)
	}
	items, ok := transferItems(parsed)
	if !ok {
		return nil, failure("invalid_transfer", "transfer file must be a JSON array or an accounts/data/records wrapper", 400)
	}
	records := make([]transferRecord, 0, len(items))
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, failure("invalid_transfer", fmt.Sprintf("transfer file item %d is not an account object", i+1), 400)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, failure("invalid_transfer", fmt.Sprintf("transfer file item %d is invalid", i+1), 400)
		}
		var record transferRecord
		if err := json.Unmarshal(encoded, &record); err != nil {
			return nil, failure("invalid_transfer", fmt.Sprintf("transfer file item %d is invalid", i+1), 400)
		}
		if strings.TrimSpace(record.AccessToken) == "" {
			// Missing tokens are valid file entries but are skipped during merge.
			records = append(records, record)
			continue
		}
		if !safeValue(record.AccessToken) || !safeValue(record.RefreshToken) || !safeValue(record.Domain) {
			return nil, failure("invalid_transfer", fmt.Sprintf("transfer file item %d contains unsafe credential text", i+1), 400)
		}
		if record.ExpiresAt < 0 || record.RefreshExpiresAt < 0 {
			return nil, failure("invalid_transfer", fmt.Sprintf("transfer file item %d has an invalid expiry", i+1), 400)
		}
		records = append(records, record)
	}
	return records, nil
}

func transferItems(parsed any) ([]any, bool) {
	if items, ok := parsed.([]any); ok {
		return items, true
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, key := range []string{"accounts", "data", "records"} {
		if items, ok := object[key].([]any); ok {
			return items, true
		}
	}
	for _, key := range []string{"data", "result"} {
		if nested, ok := object[key].(map[string]any); ok {
			for _, inner := range []string{"accounts", "data", "records"} {
				if items, ok := nested[inner].([]any); ok {
					return items, true
				}
			}
		}
	}
	return nil, false
}

func previewTransferFile(raw []byte) (transferPreview, error) {
	records, err := parseTransferFile(raw)
	if err != nil {
		return transferPreview{}, err
	}
	preview := transferPreview{Accounts: make([]transferPreviewEntry, len(records)), Total: len(records)}
	for i, record := range records {
		preview.Accounts[i] = transferPreviewEntry{
			Index: i, UID: record.UID, Nickname: record.Nickname, Email: record.Email,
			HasToken: strings.TrimSpace(record.AccessToken) != "",
		}
	}
	return preview, nil
}

func transferRecordOf(rec hostAuthRecord) (transferRecord, error) {
	stored := rec.Stored
	record := transferRecord{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		UID:          stored.UID,
		Nickname:     stored.DisplayName,
		Domain:       stored.Domain,
		ExpiresAt:    stored.ExpiresAt.UnixMilli(),
	}
	if stored.RefreshExpiresAt != nil {
		record.RefreshExpiresAt = stored.RefreshExpiresAt.UnixMilli()
	}
	record.AuthRaw = map[string]any{
		"auth": map[string]any{
			"accessToken": stored.AccessToken, "refreshToken": stored.RefreshToken,
			"expiresAt": stored.ExpiresAt.Unix(), "domain": stored.Domain,
		},
		"account": map[string]any{
			"uid": stored.UID, "nickname": stored.DisplayName, "enterpriseId": stored.EnterpriseID,
		},
	}
	if record.AccessToken == "" {
		return transferRecord{}, failure("invalid_credentials", "cannot export a credential without an access token", 400)
	}
	return record, nil
}

func credentialFromTransferRecord(record transferRecord, target site) (credential, error) {
	if strings.TrimSpace(record.AccessToken) == "" {
		return credential{}, failure("invalid_transfer", "account is missing access_token", 400)
	}
	if !safeValue(record.AccessToken) || !safeValue(record.RefreshToken) || !safeValue(record.Domain) {
		return credential{}, failure("invalid_transfer", "account contains unsafe credential text", 400)
	}
	stored := credential{
		Provider: provider, Type: provider, CredentialType: "browser", Version: credentialVersion,
		Site: target.id, UID: strings.TrimSpace(record.UID), DisplayName: strings.TrimSpace(record.Nickname),
		AccessToken: record.AccessToken, RefreshToken: record.RefreshToken, Domain: strings.TrimSpace(record.Domain),
		TokenType: "Bearer", Generation: randTextForTransfer(),
	}
	if record.ExpiresAt > 0 {
		stored.ExpiresAt = time.UnixMilli(record.ExpiresAt).UTC()
	} else {
		// The sibling format permits an omitted expiry. CPA requires a non-zero
		// expiry for browser credentials, so let the normal refresh path obtain a
		// current token shortly after import rather than rejecting the record.
		stored.ExpiresAt = time.Now().Add(5 * time.Minute).UTC()
	}
	if record.RefreshExpiresAt > 0 {
		expires := time.UnixMilli(record.RefreshExpiresAt).UTC()
		stored.RefreshExpiresAt = &expires
	}
	if raw, ok := record.AuthRaw.(map[string]any); ok {
		account, _ := raw["account"].(map[string]any)
		if enterprise, ok := account["enterpriseId"].(string); ok {
			stored.EnterpriseID = strings.TrimSpace(enterprise)
		}
	}
	if stored.UID == "" {
		return credential{}, failure("invalid_transfer", "account is missing uid", 400)
	}
	if stored.ExpiresAt.IsZero() {
		stored.ExpiresAt = time.UnixMilli(1).UTC()
	}
	return stored, nil
}

func mergeTransferRecordsAll(records []transferRecord, indexes []int) (map[string]credential, int, int) {
	changes := make(map[string]credential)
	imported, skipped := 0, 0
	for _, index := range indexes {
		if index < 0 || index >= len(records) {
			skipped++
			continue
		}
		record := records[index]
		if strings.TrimSpace(record.AccessToken) == "" {
			skipped++
			continue
		}
		region := transferRegion(record.Domain)
		target, ok := selectSite(region)
		if !ok {
			skipped++
			continue
		}
		credential, err := credentialFromTransferRecord(record, target)
		if err != nil {
			skipped++
			continue
		}
		id, _, err := credentialIdentity(credential)
		if err != nil {
			skipped++
			continue
		}
		changes[id] = credential
		imported++
	}
	return changes, imported, skipped
}

func mergeTransferRecords(records []transferRecord, indexes []int, target site) (map[string]credential, int, int) {
	changes := make(map[string]credential)
	imported, skipped := 0, 0
	for _, index := range indexes {
		if index < 0 || index >= len(records) {
			skipped++
			continue
		}
		record := records[index]
		if strings.TrimSpace(record.AccessToken) == "" {
			skipped++
			continue
		}
		if transferRegion(record.Domain) != target.id {
			skipped++
			continue
		}
		credential, err := credentialFromTransferRecord(record, target)
		if err != nil {
			skipped++
			continue
		}
		id, _, err := credentialIdentity(credential)
		if err != nil {
			skipped++
			continue
		}
		changes[id] = credential
		imported++
	}
	return changes, imported, skipped
}

func transferRegion(domain string) string {
	host := strings.ToLower(strings.TrimSpace(domain))
	if parsed, err := url.Parse(host); err == nil && parsed.Hostname() != "" {
		host = strings.ToLower(parsed.Hostname())
	}
	host = strings.TrimSuffix(host, ".")
	switch {
	case host == "copilot.tencent.com" || strings.HasSuffix(host, ".tencent.com") || host == "workbuddy.cn" || strings.HasSuffix(host, ".workbuddy.cn"):
		return "workbuddy"
	case host == "www.workbuddy.ai" || host == "workbuddy.ai" || host == "codebuddy.ai" || strings.HasSuffix(host, ".workbuddy.ai") || strings.HasSuffix(host, ".codebuddy.ai"):
		return "workbuddy-ai"
	default:
		return ""
	}
}

func randTextForTransfer() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}
