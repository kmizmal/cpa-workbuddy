package workbuddy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseTransferFileAcceptsAccountsEnvelope(t *testing.T) {
	got, err := parseTransferFile([]byte(`{"accounts":[{"access_token":"access-a","uid":"uid-a","domain":"copilot.tencent.com"}]}`))
	if err != nil {
		t.Fatalf("parseTransferFile returned error: %v", err)
	}
	if len(got) != 1 || got[0].AccessToken != "access-a" || got[0].UID != "uid-a" || got[0].problem != "" {
		t.Fatalf("unexpected records: %#v", got)
	}
}

func TestParseTransferFileDoesNotRejectFileForOneBadEntry(t *testing.T) {
	// A non-object entry and an entry with an unsafe token must not abort the
	// batch: they become per-record problems that import skips.
	got, err := parseTransferFile([]byte(`[
		{"access_token":"ok-a","uid":"uid-a","domain":"copilot.tencent.com"},
		"not-an-account",
		{"access_token":"bad\u0000token","uid":"uid-b","domain":"copilot.tencent.com"},
		{"access_token":"ok-b","uid":"uid-c","domain":"www.workbuddy.ai"}
	]`))
	if err != nil {
		t.Fatalf("parseTransferFile rejected the whole file: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 records, got %d", len(got))
	}
	if got[0].problem != "" || got[3].problem != "" {
		t.Fatalf("valid records were marked bad: %#v", got)
	}
	if !strings.Contains(got[1].problem, "item 2") || !strings.Contains(got[2].problem, "item 3") {
		t.Fatalf("bad records lost their reason: %#v", got)
	}
}

func TestParseTransferFileRejectsUnusableFiles(t *testing.T) {
	for name, input := range map[string]string{
		"empty":      "",
		"not json":   "{oops",
		"bare value": `42`,
		"no wrapper": `{"unrelated":[]}`,
	} {
		if _, err := parseTransferFile([]byte(input)); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestPreviewTransferFileRedactsTokenAndMarksProblems(t *testing.T) {
	preview, err := previewTransferFile([]byte(`[
		{"access_token":"super-secret","uid":"uid-a","nickname":"账号A","domain":"copilot.tencent.com"},
		{"uid":"uid-b","domain":"copilot.tencent.com"},
		{"access_token":"tok","uid":"uid-c","domain":"unknown.example"}
	]`))
	if err != nil {
		t.Fatalf("previewTransferFile returned error: %v", err)
	}
	if len(preview.Accounts) != 3 || preview.Total != 3 {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	if !preview.Accounts[0].HasToken || !preview.Accounts[0].Importable || preview.Accounts[0].Site != "workbuddy" {
		t.Fatalf("first entry should be importable: %#v", preview.Accounts[0])
	}
	if preview.Accounts[1].Importable || preview.Accounts[1].HasToken {
		t.Fatalf("token-less entry should be flagged: %#v", preview.Accounts[1])
	}
	if preview.Accounts[2].Importable || !strings.Contains(preview.Accounts[2].Problem, "domain") {
		t.Fatalf("unknown domain should be flagged: %#v", preview.Accounts[2])
	}
	if strings.Contains(fmt.Sprintf("%#v", preview), "super-secret") {
		t.Fatal("preview contains access token")
	}
}

func TestMergeTransferRecordsDeduplicatesAndCountsStoredCredentials(t *testing.T) {
	records := []transferRecord{
		{AccessToken: "old", UID: "same", Domain: "copilot.tencent.com"},
		{AccessToken: "new", UID: "same", Domain: "copilot.tencent.com"},
	}
	merged := mergeTransferRecords(records, []int{0, 1})
	if merged.Imported != 1 || merged.Duplicated != 1 || merged.Skipped != 0 || len(merged.Changes) != 1 {
		t.Fatalf("unexpected merge counts: imported=%d duplicated=%d skipped=%d changes=%d",
			merged.Imported, merged.Duplicated, merged.Skipped, len(merged.Changes))
	}
	if merged.Imported != len(merged.Changes) {
		t.Fatal("imported must equal the number of credentials actually stored")
	}
	for _, credential := range merged.Changes {
		if credential.AccessToken != "new" {
			t.Fatalf("duplicate did not use last record: %#v", credential)
		}
	}
}

func TestMergeTransferRecordsSkipsBadEntriesIndividually(t *testing.T) {
	records, err := parseTransferFile([]byte(`[
		{"access_token":"cn","uid":"uid-cn","domain":"copilot.tencent.com"},
		{"access_token":"global","uid":"uid-global","domain":"www.workbuddy.ai"},
		{"access_token":"unknown","uid":"uid-unknown","domain":"unknown.example"},
		{"uid":"uid-no-token","domain":"copilot.tencent.com"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeTransferRecords(records, []int{0, 1, 2, 3, 9})
	if len(merged.Changes) != 2 || merged.Imported != 2 || merged.Skipped != 3 {
		t.Fatalf("unexpected mixed merge counts: changes=%d imported=%d skipped=%d",
			len(merged.Changes), merged.Imported, merged.Skipped)
	}
}

func TestTransferRecordOfOmitsZeroExpirySoExportIsReimportable(t *testing.T) {
	rec := hostAuthRecord{Stored: credential{AccessToken: "access-a", UID: "uid-a", Domain: "copilot.tencent.com"}}
	got, err := transferRecordOf(rec)
	if err != nil {
		t.Fatalf("transferRecordOf returned error: %v", err)
	}
	if got.ExpiresAt != 0 || got.RefreshExpiresAt != 0 {
		t.Fatalf("zero expiry must stay omitted, got expiresAt=%d refreshExpiresAt=%d", got.ExpiresAt, got.RefreshExpiresAt)
	}
	// Round trip through JSON as the panel would.
	preview, err := previewTransferFile(mustJSON(t, map[string]any{"accounts": []transferRecord{got}}))
	if err != nil {
		t.Fatalf("exported record could not be re-imported: %v", err)
	}
	if !preview.Accounts[0].Importable {
		t.Fatalf("exported record is not importable: %#v", preview.Accounts[0])
	}
}

func TestTransferRecordOfUsesReferenceFieldsAndMilliseconds(t *testing.T) {
	expires := time.UnixMilli(1_725_123_456_789).UTC()
	rec := hostAuthRecord{Stored: credential{AccessToken: "access-a", RefreshToken: "refresh-a", UID: "uid-a", DisplayName: "账号A", Domain: "copilot.tencent.com", ExpiresAt: expires}}
	got, err := transferRecordOf(rec)
	if err != nil {
		t.Fatalf("transferRecordOf returned error: %v", err)
	}
	if got.AccessToken != "access-a" || got.UID != "uid-a" || got.ExpiresAt != expires.UnixMilli() {
		t.Fatalf("unexpected export record: %#v", got)
	}
	if !strings.Contains(fmt.Sprintf("%#v", got.AuthRaw), "auth") {
		t.Fatalf("export lost auth_raw: %#v", got.AuthRaw)
	}
}

func TestTransferRecordRoundTripPreservesMillisecondExpiry(t *testing.T) {
	original := time.UnixMilli(1_725_123_456_789).UTC()
	record := transferRecord{AccessToken: "token", UID: "uid", Domain: "copilot.tencent.com", ExpiresAt: original.UnixMilli()}
	credential, err := credentialFromTransferRecord(record, mustSite("workbuddy"))
	if err != nil {
		t.Fatalf("credentialFromTransferRecord returned error: %v", err)
	}
	if credential.ExpiresAt.UnixMilli() != original.UnixMilli() {
		t.Fatalf("expiry changed: got %d want %d", credential.ExpiresAt.UnixMilli(), original.UnixMilli())
	}
}

// A credential whose stored domain is not one the importer recognises must
// still export as a file the importer accepts: otherwise the round trip that
// the transfer format exists for silently breaks on that one account.
func TestTransferRecordOfKeepsUnknownDomainExportImportable(t *testing.T) {
	rec := hostAuthRecord{
		Site:   mustSite("workbuddy"),
		Stored: credential{AccessToken: "access-a", UID: "uid-a", Domain: "account.workbuddy"},
	}
	exported, err := transferRecordOf(rec)
	if err != nil {
		t.Fatalf("transferRecordOf returned error: %v", err)
	}
	records, err := parseTransferFile(mustJSON(t, []transferRecord{exported}))
	if err != nil {
		t.Fatalf("parseTransferFile returned error: %v", err)
	}
	if records[0].problem != "" {
		t.Fatalf("exported record cannot be re-imported: %s", records[0].problem)
	}
	if got := transferRegion(transferDomain(records[0])); got != "workbuddy" {
		t.Fatalf("re-import resolved site %q, want workbuddy", got)
	}
	// The exported value becomes the X-Domain request header, so it must be a
	// bare hostname. A scheme-bearing URL there breaks every upstream call the
	// re-imported account makes.
	if domain := transferDomain(records[0]); strings.Contains(domain, "/") || strings.Contains(domain, ":") {
		t.Fatalf("exported domain is not a bare hostname: %q", domain)
	}
}

// The nested auth_raw document is the reference exporter's shape, so its
// seconds-based expiry must stay a real timestamp: a zero expiry must be
// omitted rather than serialised as the zero time's negative Unix seconds.
func TestTransferRecordOfOmitsZeroExpiryFromAuthRaw(t *testing.T) {
	rec := hostAuthRecord{
		Site:   mustSite("workbuddy"),
		Stored: credential{AccessToken: "access-a", UID: "uid-a", Domain: "copilot.tencent.com"},
	}
	exported, err := transferRecordOf(rec)
	if err != nil {
		t.Fatalf("transferRecordOf returned error: %v", err)
	}
	auth, ok := exported.AuthRaw.(map[string]any)["auth"].(map[string]any)
	if !ok {
		t.Fatalf("export lost the nested auth document: %#v", exported.AuthRaw)
	}
	if seconds, ok := auth["expiresAt"].(int64); ok && seconds < 0 {
		t.Fatalf("auth_raw carries a negative expiry: %d", seconds)
	}
}

// An external file may spell the domain as a URL. The value is stored as the
// account's X-Domain header, so import must reduce it to a hostname instead of
// persisting whatever the file said.
func TestCredentialFromTransferRecordStoresBareHostname(t *testing.T) {
	for _, domain := range []string{
		"https://copilot.tencent.com",
		"https://copilot.tencent.com/path",
		"copilot.tencent.com.",
		"HTTPS://Copilot.Tencent.Com",
	} {
		record := transferRecord{AccessToken: "token", UID: "uid", Domain: domain}
		credential, err := credentialFromTransferRecord(record, mustSite("workbuddy"))
		if err != nil {
			t.Fatalf("%s: credentialFromTransferRecord returned error: %v", domain, err)
		}
		if credential.Domain != "copilot.tencent.com" {
			t.Fatalf("%s: stored domain %q, want the bare hostname", domain, credential.Domain)
		}
	}
}

// A domain recovered from auth_raw must pass the same safety check as the
// flat field. Otherwise a CR/LF smuggled in there reaches the host write path
// and, because the merge step accepts it, the batch fails part-way through
// after some credentials were already stored.
func TestParseTransferFileRejectsUnsafeAuthRawDomain(t *testing.T) {
	records, err := parseTransferFile([]byte(`[
		{"access_token":"ok","uid":"uid-a","auth_raw":{"auth":{"domain":"copilot.tencent.com\r\nX-Evil: 1"}}}
	]`))
	if err != nil {
		t.Fatalf("parseTransferFile returned error: %v", err)
	}
	if records[0].problem == "" {
		t.Fatalf("unsafe auth_raw domain was accepted: %#v", records[0])
	}
	if !strings.Contains(records[0].problem, "unsafe") {
		t.Fatalf("unexpected problem text: %q", records[0].problem)
	}
}

// The preview and merge stages must accept the same identities. A UID or
// enterprise ID with a CR/LF/NUL is rejected by credentialSite during merge;
// leaving it unmarked during preview makes a record look importable and then
// silently skips it at confirmation time.
func TestParseTransferFileRejectsUnsafeUIDAndEnterpriseID(t *testing.T) {
	cases := map[string]string{
		"unsafe uid":        `{"access_token":"ok","uid":"uid\r\nX-Evil: 1","domain":"copilot.tencent.com"}`,
		"unsafe enterprise": `{"access_token":"ok","uid":"uid-a","domain":"copilot.tencent.com","auth_raw":{"account":{"uid":"uid-a","enterpriseId":"team\u0000one"}}}`,
	}
	for name, raw := range cases {
		records, err := parseTransferFile([]byte("[" + raw + "]"))
		if err != nil {
			t.Fatalf("%s: parseTransferFile returned error: %v", name, err)
		}
		if records[0].problem == "" {
			t.Fatalf("%s: unsafe identity was accepted: %#v", name, records[0])
		}
		if !strings.Contains(records[0].problem, "unsafe") {
			t.Fatalf("%s: unexpected problem text: %q", name, records[0].problem)
		}
	}
}

// Every record must be validated before the first host write. A record that
// only fails while normalizing would otherwise surface as a mid-batch error
// with an arbitrary subset of the batch already stored.
func TestMergeTransferRecordsSkipsRecordsThatCannotNormalize(t *testing.T) {
	records := []transferRecord{
		{AccessToken: "ok", UID: "uid-good", Domain: "copilot.tencent.com"},
		{AccessToken: "bad\x00token", UID: "uid-bad", Domain: "copilot.tencent.com"},
	}
	merged := mergeTransferRecords(records, []int{0, 1})
	if merged.Imported != 1 || merged.Skipped != 1 {
		t.Fatalf("unexpected merge result: %#v", merged)
	}
}

// A record whose auth_raw carries an `auth` object WITHOUT a domain must still
// resolve: real desktop auth documents hold `accessToken`/`baseUrl` there and
// name no domain at all, and a sibling exporter may scrub the field. Resolving
// those to "" made the importer reject a valid credential with
// "item 1 has an unsupported domain" (the site is known from the flat field or
// from auth_raw's root).
func TestTransferDomainFallsBackWhenAuthRawAuthHasNoDomain(t *testing.T) {
	cases := map[string]struct {
		record string
		want   string
	}{
		"auth object without domain, flat domain present": {
			record: `{"access_token":"t","uid":"u","domain":"www.workbuddy.cn","auth_raw":{"auth":{"accessToken":"t"},"account":{"uid":"u"}}}`,
			want:   "www.workbuddy.cn",
		},
		"auth object with only baseUrl, root domain present": {
			record: `{"access_token":"t","uid":"u","auth_raw":{"auth":{"baseUrl":"https://copilot.tencent.com"},"domain":"www.codebuddy.cn","uid":"u"}}`,
			want:   "www.codebuddy.cn",
		},
		"auth object without domain, root domain in auth_raw": {
			record: `{"access_token":"t","uid":"u","auth_raw":{"auth":{"accessToken":"t"},"domain":"copilot.tencent.com"}}`,
			want:   "copilot.tencent.com",
		},
		"baseUrl only, nothing else": {
			record: `{"access_token":"t","uid":"u","auth_raw":{"auth":{"baseUrl":"https://www.workbuddy.cn/login"},"account":{"uid":"u"}}}`,
			want:   "www.workbuddy.cn",
		},
	}
	for name, tc := range cases {
		records, err := parseTransferFile([]byte("[" + tc.record + "]"))
		if err != nil {
			t.Fatalf("%s: parseTransferFile returned error: %v", name, err)
		}
		if got := transferDomain(records[0]); got != tc.want {
			t.Fatalf("%s: transferDomain = %q, want %q", name, got, tc.want)
		}
		if records[0].problem != "" {
			t.Fatalf("%s: record was rejected: %s", name, records[0].problem)
		}
	}
}

// Real export files do not always carry a bare hostname: sibling tools store the
// login URL, and a credential copied out of a network capture can name a port.
// Every one of those must reduce to the hostname, because the value ends up in
// the X-Domain header and a leftover scheme/port/path breaks every call the
// account makes -- and, before normalisation, rejected the whole record as an
// unsupported domain.
// The two ways a record can fail the domain gate are different problems for the
// user, and the message has to say which: a file with no domain at all needs a
// different fix (add `domain`) than a file naming a host this build does not
// serve. Reporting both as "unsupported domain" sent the account-transfer bug
// report hunting a domain that was never present.
func TestTransferRecordProblemDistinguishesMissingFromUnknownDomain(t *testing.T) {
	missing := transferRecord{AccessToken: "t", UID: "u"}
	if got := transferRecordProblem(missing, 1); !strings.Contains(got, "missing domain") {
		t.Fatalf("record with no domain reported %q, want a missing-domain message", got)
	}
	unknown := transferRecord{AccessToken: "t", UID: "u", Domain: "login.example.invalid"}
	if got := transferRecordProblem(unknown, 2); !strings.Contains(got, "unsupported domain") {
		t.Fatalf("record with an unknown domain reported %q, want an unsupported-domain message", got)
	}
}

func TestTransferDomainNormalizesHostPortAndPathForms(t *testing.T) {
	cases := map[string]string{
		"copilot.tencent.com":                                          "copilot.tencent.com",
		"https://copilot.tencent.com":                                  "copilot.tencent.com",
		"https://copilot.tencent.com/":                                 "copilot.tencent.com",
		"https://copilot.tencent.com/login":                            "copilot.tencent.com",
		"https://copilot.tencent.com/login?platform=workbuddy&state=x": "copilot.tencent.com",
		"copilot.tencent.com:443":                                      "copilot.tencent.com",
		"https://copilot.tencent.com:443":                              "copilot.tencent.com",
		"copilot.tencent.com/path":                                     "copilot.tencent.com",
		"//copilot.tencent.com":                                        "copilot.tencent.com",
		"COPILOT.TENCENT.COM":                                          "copilot.tencent.com",
		"  copilot.tencent.com  ":                                      "copilot.tencent.com",
		"copilot.tencent.com.":                                         "copilot.tencent.com",
		"https://www.workbuddy.cn/login?x=1":                           "www.workbuddy.cn",
		"www.codebuddy.cn:8443":                                        "www.codebuddy.cn",
		"https://user@www.workbuddy.ai/x":                              "www.workbuddy.ai",
		"":                                                             "",
		"   ":                                                          "",
		"copilot.tencent.com.evil.invalid":                             "copilot.tencent.com.evil.invalid",
	}
	for raw, want := range cases {
		if got := normalizeTransferDomain(raw); got != want {
			t.Errorf("normalizeTransferDomain(%q) = %q, want %q", raw, got, want)
		}
	}
	// The normalised value is what the region gate and the X-Domain header see.
	for _, raw := range []string{"copilot.tencent.com:443", "https://copilot.tencent.com/login"} {
		if got := transferRegion(raw); got != "workbuddy" {
			t.Errorf("transferRegion(%q) = %q, want workbuddy", raw, got)
		}
	}
}

func TestTransferRegionResolvesSitesAndRejectsUnknownDomains(t *testing.T) {
	cases := map[string]string{
		"copilot.tencent.com":              "workbuddy",
		"https://copilot.tencent.com/path": "workbuddy",
		"www.workbuddy.cn":                 "workbuddy",
		"www.codebuddy.cn":                 "workbuddy",
		"codebuddy.cn":                     "workbuddy",
		"www.workbuddy.ai":                 "workbuddy-ai",
		"codebuddy.ai":                     "workbuddy-ai",
		"unknown.example":                  "",
		// Subdomains are not WorkBuddy hosts: accepting them would let an
		// arbitrary host become the X-Domain of an imported credential.
		"evil.tencent.com":        "",
		"foo.workbuddy.cn":        "",
		"foo.codebuddy.cn":        "",
		"foo.workbuddy.ai":        "",
		"foo.codebuddy.ai":        "",
		"sub.copilot.tencent.com": "",
		"":                        "",
	}
	for domain, want := range cases {
		if got := transferRegion(domain); got != want {
			t.Errorf("transferRegion(%q) = %q, want %q", domain, got, want)
		}
	}
}

// A credential holding a CN account commonly names the CodeBuddy brand
// domain, and the reference exporter nests it under auth_raw.auth.domain.
// Both spellings and both placements must import, or the plugin refuses the
// files it and the sibling tools produce ("item 1 has an unsupported domain").
func TestTransferImportAcceptsBothCNBrandDomains(t *testing.T) {
	cases := map[string]string{
		"flat codebuddy.cn":   `{"access_token":"t1","uid":"uid-1","domain":"www.codebuddy.cn"}`,
		"flat workbuddy.cn":   `{"access_token":"t2","uid":"uid-2","domain":"www.workbuddy.cn"}`,
		"nested codebuddy.cn": `{"access_token":"t3","auth_raw":{"auth":{"domain":"www.codebuddy.cn"},"account":{"uid":"uid-3"}}}`,
		"nested workbuddy.cn": `{"access_token":"t4","auth_raw":{"auth":{"domain":"codebuddy.cn"},"account":{"uid":"uid-4"}}}`,
		// A sibling tool may copy a desktop auth document verbatim, which puts
		// the fields at the root of auth_raw instead of nesting them.
		"desktop codebuddy.cn": `{"access_token":"t5","auth_raw":{"domain":"www.codebuddy.cn","uid":"uid-5"}}`,
	}
	for name, record := range cases {
		records, err := parseTransferFile([]byte("[" + record + "]"))
		if err != nil {
			t.Fatalf("%s: parseTransferFile returned error: %v", name, err)
		}
		if records[0].problem != "" {
			t.Fatalf("%s: record was rejected: %s", name, records[0].problem)
		}
		if got := transferRegion(transferDomain(records[0])); got != "workbuddy" {
			t.Fatalf("%s: resolved site %q, want workbuddy", name, got)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustSite(id string) site {
	got, ok := selectSite(id)
	if !ok {
		panic("unknown test site: " + id)
	}
	return got
}
