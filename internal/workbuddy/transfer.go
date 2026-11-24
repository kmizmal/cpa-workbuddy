package workbuddy

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// transferRecord is the JSON contract shared with dsh-connect-workbuddy.
//
// A record that cannot be imported does not abort the whole file: the parser
// keeps it and records the reason in problem so preview can show it and the
// merge step can skip it while still importing the remaining entries.
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

	problem string
}

type transferPreviewEntry struct {
	Index      int    `json:"index"`
	UID        string `json:"uid,omitempty"`
	Nickname   string `json:"nickname,omitempty"`
	Email      string `json:"email,omitempty"`
	Site       string `json:"site,omitempty"`
	HasToken   bool   `json:"has_token"`
	Importable bool   `json:"importable"`
	Problem    string `json:"problem,omitempty"`
}

type transferPreview struct {
	Accounts []transferPreviewEntry `json:"accounts"`
	Total    int                    `json:"total"`
}

// transferMerge is the outcome of merging selected records into credentials.
// Imported always equals len(Changes): it counts credentials actually written,
// not input records, so duplicate identities cannot inflate it.
type transferMerge struct {
	Changes    map[string]credential
	Imported   int
	Skipped    int
	Duplicated int
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
		position := i + 1
		object, ok := item.(map[string]any)
		if !ok {
			records = append(records, transferRecord{problem: fmt.Sprintf("item %d is not an account object", position)})
			continue
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			records = append(records, transferRecord{problem: fmt.Sprintf("item %d cannot be encoded", position)})
			continue
		}
		var record transferRecord
		if err := json.Unmarshal(encoded, &record); err != nil {
			records = append(records, transferRecord{problem: fmt.Sprintf("item %d has an invalid field type", position)})
			continue
		}
		record.problem = transferRecordProblem(record, position)
		records = append(records, record)
	}
	return records, nil
}

// transferDomain resolves the domain a record belongs to, falling back to the
// nested auth_raw.auth.domain used by the reference exporter when the flat
// field is absent.
func transferDomain(record transferRecord) string {
	if domain := normalizeTransferDomain(record.Domain); domain != "" {
		return domain
	}
	root := authRawRoot(record)
	if root == nil {
		return ""
	}
	auth, _ := root["auth"].(map[string]any)
	// Every source is TRIED IN TURN rather than returning on the first hit: an
	// auth document legitimately carries an `auth` object that names no domain
	// (a desktop file holds accessToken/baseUrl there, and the reference vault
	// serializes domain as "" when the login never reported one). Returning early
	// on the presence of the KEY turned a missing value into a permanent "" and
	// rejected a credential whose domain sat one level up, which is the
	// "item 1 has an unsupported domain" report this fallback chain fixes.
	//
	// An explicit `domain` outranks a `baseUrl` at ANY level: the base is the host
	// the client happened to talk to (a failover entry can point at a different
	// brand host), while `domain` is the sign-in the credential belongs to.
	explicit := []string{stringField(root, "domain"), stringField(auth, "domain")}
	for _, domain := range explicit {
		if resolved := normalizeTransferDomain(domain); resolved != "" {
			return resolved
		}
	}
	// Only when nothing names a domain does the recorded gateway decide, because
	// for a desktop document it is the only evidence of which product this is.
	for _, base := range []string{stringField(root, "baseUrl"), stringField(auth, "baseUrl")} {
		if resolved := normalizeTransferDomain(base); resolved != "" {
			return resolved
		}
	}
	return ""
}

// stringField reads one string member of a decoded JSON object, tolerating a nil
// (absent) document and a non-string value.
func stringField(document map[string]any, key string) string {
	if document == nil {
		return ""
	}
	value, _ := document[key].(string)
	return value
}

// normalizeTransferDomain reduces a domain to the bare hostname that the
// upstream request path expects in X-Domain. The value lands in
// credential.Domain, and requestDomain passes a non-empty value through
// verbatim, so a scheme or path left here would be sent to the upstream
// verbatim and break every call the account makes.
func normalizeTransferDomain(domain string) string {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return ""
	}
	// A BARE host with a port or path ("copilot.tencent.com:443",
	// "copilot.tencent.com/login") does not parse to a Hostname: url.Parse reads
	// the first segment as a scheme-less path, so Hostname() comes back empty and
	// the value fell through verbatim -- which then failed the region gate AND,
	// once stored, would have been sent to the upstream as X-Domain. Prefixing the
	// scheme is what makes those forms parse; a value that already carries one is
	// left alone.
	candidate := trimmed
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + strings.TrimPrefix(candidate, "//")
	}
	if parsed, err := url.Parse(candidate); err == nil && parsed.Hostname() != "" {
		return strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	}
	// Not URL-shaped at all: strip the decoration a bare token can still carry
	// (a port, a path, a trailing dot) rather than handing back the whole thing.
	if host, _, ok := strings.Cut(trimmed, "/"); ok {
		trimmed = host
	}
	if host, _, ok := strings.Cut(trimmed, ":"); ok {
		trimmed = host
	}
	return strings.ToLower(strings.TrimSuffix(trimmed, "."))
}

// authRawRoot returns the auth_raw document as an object, or nil when the
// record carries none. Both the nested {auth, account} shape this plugin
// exports and a flat desktop auth document are honoured.
func authRawRoot(record transferRecord) map[string]any {
	root, _ := record.AuthRaw.(map[string]any)
	return root
}

// transferRecordProblem reports why a parsed record cannot be imported, or ""
// when it is usable. Missing tokens stay a per-record problem rather than a
// file-level error so one bad entry never blocks a batch.
func transferRecordProblem(record transferRecord, position int) string {
	if strings.TrimSpace(record.AccessToken) == "" {
		return fmt.Sprintf("item %d is missing access_token", position)
	}
	// The domain may come from auth_raw instead of the flat field, so check the
	// resolved value: a CR/LF smuggled into the nested document would otherwise
	// reach the host write path and fail the batch part-way through, after some
	// credentials were already stored.
	domain := transferDomain(record)
	if !safeValue(record.AccessToken) || !safeValue(record.RefreshToken) || !safeValue(record.Domain) || !safeValue(domain) {
		return fmt.Sprintf("item %d contains unsafe credential text", position)
	}
	// The merge path validates the final UID and enterprise ID with safeValue
	// through credentialSite. Check the same raw identities during preview so a
	// record cannot look importable and then be silently skipped on confirm.
	account := authRawAccount(record)
	uid := record.UID
	if strings.TrimSpace(uid) == "" && account != nil {
		uid, _ = account["uid"].(string)
	}
	enterpriseID := ""
	if account != nil {
		enterpriseID, _ = account["enterpriseId"].(string)
	}
	if !safeValue(uid) || !safeValue(enterpriseID) {
		return fmt.Sprintf("item %d contains unsafe credential text", position)
	}
	if record.ExpiresAt < 0 || record.RefreshExpiresAt < 0 {
		return fmt.Sprintf("item %d has a negative expiry", position)
	}
	if strings.TrimSpace(uid) == "" {
		return fmt.Sprintf("item %d is missing uid", position)
	}
	// Two different problems, two different fixes: a record naming no domain at
	// all (the common shape of a desktop auth document, which carries the token
	// and a baseUrl but no domain) needs `domain` added, while one naming a host
	// this build does not serve is a genuinely foreign credential. Collapsing both
	// into "unsupported domain" sent the bug report hunting a value that was
	// never there.
	if domain == "" {
		return fmt.Sprintf("item %d is missing domain", position)
	}
	if _, ok := selectSite(transferRegion(domain)); !ok {
		return fmt.Sprintf("item %d has an unsupported domain", position)
	}
	return ""
}

func authRawAccount(record transferRecord) map[string]any {
	root := authRawRoot(record)
	if root == nil {
		return nil
	}
	if account, ok := root["account"].(map[string]any); ok {
		return account
	}
	// Flat desktop auth documents put uid/nickname/enterpriseId at the root.
	if _, ok := root["uid"]; ok {
		return root
	}
	if _, ok := root["nickname"]; ok {
		return root
	}
	if _, ok := root["enterpriseId"]; ok {
		return root
	}
	return nil
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
		problem := record.problem
		preview.Accounts[i] = transferPreviewEntry{
			Index: i, UID: record.UID, Nickname: record.Nickname, Email: record.Email,
			Site: transferRegion(transferDomain(record)), HasToken: strings.TrimSpace(record.AccessToken) != "",
			Importable: problem == "", Problem: problem,
		}
	}
	return preview, nil
}

func transferRecordOf(rec hostAuthRecord) (transferRecord, error) {
	stored := rec.Stored
	if stored.AccessToken == "" {
		return transferRecord{}, failure("invalid_credentials", "cannot export a credential without an access token", 400)
	}
	record := transferRecord{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		UID:          stored.UID,
		Nickname:     stored.DisplayName,
		Domain:       stored.Domain,
	}
	// Older credentials can carry no domain even though their site is known,
	// and an upstream may report a domain the importer does not recognise. In
	// both cases export the site endpoint: a file the importer would reject
	// defeats the purpose of exporting it.
	// The endpoint carries a scheme; export the hostname so the value is usable
	// as the X-Domain header once the file is re-imported.
	if transferRegion(record.Domain) == "" {
		record.Domain = strings.TrimPrefix(rec.Site.endpoint, "https://")
	}
	// A zero expiry must stay omitted: UnixMilli() on the zero time is a large
	// negative number, and the importer rejects negative expiries, which would
	// make exported files impossible to re-import.
	if !stored.ExpiresAt.IsZero() {
		record.ExpiresAt = stored.ExpiresAt.UnixMilli()
	}
	if stored.RefreshExpiresAt != nil && !stored.RefreshExpiresAt.IsZero() {
		record.RefreshExpiresAt = stored.RefreshExpiresAt.UnixMilli()
	}
	auth := map[string]any{
		"accessToken": stored.AccessToken, "refreshToken": stored.RefreshToken,
		"domain": record.Domain,
	}
	// Same rule as the flat expiry: the zero time's Unix seconds are a large
	// negative number, so an unknown expiry stays absent.
	if !stored.ExpiresAt.IsZero() {
		auth["expiresAt"] = stored.ExpiresAt.Unix()
	}
	record.AuthRaw = map[string]any{
		"auth": auth,
		"account": map[string]any{
			"uid": stored.UID, "nickname": stored.DisplayName, "enterpriseId": stored.EnterpriseID,
		},
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
		AccessToken: record.AccessToken, RefreshToken: record.RefreshToken, Domain: transferDomain(record),
		TokenType: "Bearer", Generation: rand.Text(),
	}
	if stored.UID == "" {
		uid, _ := authRawAccount(record)["uid"].(string)
		stored.UID = strings.TrimSpace(uid)
	}
	if stored.DisplayName == "" {
		nickname, _ := authRawAccount(record)["nickname"].(string)
		stored.DisplayName = strings.TrimSpace(nickname)
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
	if enterprise, ok := authRawAccount(record)["enterpriseId"].(string); ok {
		stored.EnterpriseID = strings.TrimSpace(enterprise)
	}
	if stored.UID == "" {
		return credential{}, failure("invalid_transfer", "account is missing uid", 400)
	}
	return stored, nil
}

// mergeImportedCredential applies an imported record over an existing
// host-owned credential without discarding fields the transfer file does not
// carry. Token fields are replaced by the import; refresh metadata, display
// name and cached model snapshot only change when the import supplies a value,
// and the host-owned filename is always kept.
func mergeImportedCredential(base, next credential) credential {
	merged := mergeCredentialTokens(base, next)
	if next.ModelSnapshot == nil {
		merged.ModelSnapshot = base.ModelSnapshot
		merged.ModelDiscoveryFailure = base.ModelDiscoveryFailure
	}
	if strings.TrimSpace(next.RefreshToken) == "" {
		merged.RefreshToken = base.RefreshToken
	}
	if next.RefreshExpiresAt == nil {
		merged.RefreshExpiresAt = base.RefreshExpiresAt
	}
	if strings.TrimSpace(next.DisplayName) != "" {
		merged.DisplayName = next.DisplayName
	}
	return merged
}

// mergeTransferRecords turns the selected records into credentials keyed by
// stable identity. Records that cannot be imported are skipped individually, so
// one malformed entry never rejects the rest of the batch.
func mergeTransferRecords(records []transferRecord, indexes []int) transferMerge {
	merged := transferMerge{Changes: make(map[string]credential, len(indexes))}
	seen := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		if index < 0 || index >= len(records) {
			merged.Skipped++
			continue
		}
		// A selection may repeat an index; counting it twice would make
		// imported+skipped+duplicated disagree with the number the panel said
		// the user selected.
		if seen[index] {
			continue
		}
		seen[index] = true
		record := records[index]
		if record.problem != "" {
			merged.Skipped++
			continue
		}
		target, ok := selectSite(transferRegion(transferDomain(record)))
		if !ok {
			merged.Skipped++
			continue
		}
		credential, err := credentialFromTransferRecord(record, target)
		if err != nil {
			merged.Skipped++
			continue
		}
		// Validate here, not in the write loop: a credential the host would
		// reject must be a skipped record, otherwise the batch fails after an
		// arbitrary subset was already stored.
		if _, err := credentialSite(credential); err != nil {
			merged.Skipped++
			continue
		}
		id, _, err := credentialIdentity(credential)
		if err != nil {
			merged.Skipped++
			continue
		}
		if _, exists := merged.Changes[id]; exists {
			merged.Duplicated++
		}
		merged.Changes[id] = credential
	}
	merged.Imported = len(merged.Changes)
	return merged
}

// importTransferResult is returned by the transfer import endpoint.
type importTransferResult struct {
	Imported   int `json:"imported"`
	Skipped    int `json:"skipped"`
	Duplicated int `json:"duplicated"`
}

// importProgressError keeps the number of credentials already persisted in the
// error message, because HandleManagement cannot return a partial result body
// together with an error.
func importProgressError(err error, written int) error {
	message := fmt.Sprintf("imported %d credential(s) before the import stopped: %v", written, err)
	if detail, ok := err.(*Error); ok {
		clone := *detail
		clone.Message = message
		return &clone
	}
	return failure("host_save_failed", message, 502)
}

// importTransferRecords owns the whole import orchestration: parse, select,
// match existing host credentials by identity, then persist. Keeping it out of
// HandleManagement leaves that dispatcher as routing only.
//
// A host read failure is an error, never a silent skip: treating an unreadable
// existing credential as absent would create a duplicate file and drop the
// host-managed metadata the existing record carries.
func importTransferRecords(call func(string, []byte) ([]byte, error), text string, indexes []int) (importTransferResult, error) {
	records, err := parseTransferFile([]byte(text))
	if err != nil {
		return importTransferResult{}, err
	}
	if len(indexes) == 0 {
		return importTransferResult{}, failure("invalid_transfer", "indexes must contain at least one record", 400)
	}
	merged := mergeTransferRecords(records, indexes)
	result := importTransferResult{Imported: merged.Imported, Skipped: merged.Skipped, Duplicated: merged.Duplicated}
	if len(merged.Changes) == 0 {
		return result, nil
	}

	entries, err := hostAuthList(call)
	if err != nil {
		return importTransferResult{}, err
	}
	existing := make(map[string]hostAuthRecord, len(entries))
	for _, entry := range entries {
		rec, getErr := hostAuthGet(call, entry.AuthIndex)
		if getErr != nil {
			return importTransferResult{}, getErr
		}
		id, _, idErr := credentialIdentity(rec.Stored)
		if idErr != nil {
			continue
		}
		existing[id] = rec
	}

	// Sorted so a partial failure is reproducible and the reported count names
	// the credentials that really landed, not an arbitrary prefix of a map walk.
	ids := make([]string, 0, len(merged.Changes))
	for id := range merged.Changes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	written := 0
	for _, id := range ids {
		next := merged.Changes[id]
		rec, exists := existing[id]
		var auth AuthData
		var authErr error
		if exists {
			mergedCredential := mergeImportedCredential(rec.Stored, next)
			if rec.FileName == "" {
				authErr = failure("host_save_failed", "existing credential is missing the host file name", 502)
			} else {
				// Use the same basename as hostAuthSaveCredential, even if an
				// older import left a different name in the stored JSON.
				mergedCredential.FileName = filepath.Base(rec.FileName)
				auth, authErr = authFromCredential(mergedCredential, rec.Auth.Metadata)
			}
		} else {
			auth, authErr = authFromCredential(next, nil)
		}
		if authErr != nil {
			return importTransferResult{Imported: written, Skipped: result.Skipped, Duplicated: result.Duplicated}, importProgressError(authErr, written)
		}
		var saveErr error
		if exists {
			saveErr = hostAuthSaveCredential(call, rec, auth)
		} else {
			saveErr = hostAuthSaveNewCredential(call, auth)
		}
		if saveErr != nil {
			// Report how many were stored before the failure: the client cannot
			// otherwise tell whether retrying would duplicate accounts.
			return importTransferResult{Imported: written, Skipped: result.Skipped, Duplicated: result.Duplicated}, importProgressError(saveErr, written)
		}
		written++
	}
	result.Imported = written
	return result, nil
}

func transferRegion(domain string) string {
	host := normalizeTransferDomain(domain)
	// Only the exact product hosts are trusted. A generic suffix check would
	// accept arbitrary subdomains such as evil.tencent.com, and the value is
	// stored as credential.Domain and sent upstream as X-Domain.
	switch host {
	case "copilot.tencent.com",
		"workbuddy.cn", "www.workbuddy.cn",
		"codebuddy.cn", "www.codebuddy.cn":
		return "workbuddy"
	case "workbuddy.ai", "www.workbuddy.ai",
		"codebuddy.ai", "www.codebuddy.ai":
		return "workbuddy-ai"
	default:
		return ""
	}
}
