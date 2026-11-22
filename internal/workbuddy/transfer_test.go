package workbuddy

import (
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
	if len(got) != 1 || got[0].AccessToken != "access-a" || got[0].UID != "uid-a" {
		t.Fatalf("unexpected records: %#v", got)
	}
}

func TestParseTransferFileRejectsMalformedEntryWithPosition(t *testing.T) {
	_, err := parseTransferFile([]byte(`[{"access_token":"ok"},"not-an-account"]`))
	if err == nil || !strings.Contains(err.Error(), "item 2") {
		t.Fatalf("expected positional error, got %v", err)
	}
}

func TestPreviewTransferFileRedactsToken(t *testing.T) {
	preview, err := previewTransferFile([]byte(`[{"access_token":"super-secret","uid":"uid-a","nickname":"账号A"}]`))
	if err != nil {
		t.Fatalf("previewTransferFile returned error: %v", err)
	}
	if len(preview.Accounts) != 1 || !preview.Accounts[0].HasToken {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	if strings.Contains(fmt.Sprintf("%#v", preview), "super-secret") {
		t.Fatal("preview contains access token")
	}
}

func TestMergeTransferRecordsDeduplicatesByCredentialIdentity(t *testing.T) {
	cn := mustSite("workbuddy")
	records := []transferRecord{
		{AccessToken: "old", UID: "same", Domain: "copilot.tencent.com"},
		{AccessToken: "new", UID: "same", Domain: "copilot.tencent.com"},
	}
	changes, imported, skipped := mergeTransferRecords(records, []int{0, 1}, cn)
	if imported != 2 || skipped != 0 || len(changes) != 1 {
		t.Fatalf("unexpected merge counts: changes=%d imported=%d skipped=%d", len(changes), imported, skipped)
	}
	for _, credential := range changes {
		if credential.AccessToken != "new" {
			t.Fatalf("duplicate did not use last record: %#v", credential)
		}
	}
}

func TestMergeTransferRecordsRejectsUnknownRegion(t *testing.T) {
	cn := mustSite("workbuddy")
	records := []transferRecord{{AccessToken: "token", UID: "uid", Domain: "unknown.example"}}
	changes, imported, skipped := mergeTransferRecords(records, []int{0}, cn)
	if len(changes) != 0 || imported != 0 || skipped != 1 {
		t.Fatalf("unknown domain was imported: changes=%d imported=%d skipped=%d", len(changes), imported, skipped)
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

func TestMergeTransferRecordsAllCountsMixedAndUnknownOnce(t *testing.T) {
	records := []transferRecord{
		{AccessToken: "cn", UID: "uid-cn", Domain: "copilot.tencent.com"},
		{AccessToken: "global", UID: "uid-global", Domain: "www.workbuddy.ai"},
		{AccessToken: "unknown", UID: "uid-unknown", Domain: "unknown.example"},
	}
	changes, imported, skipped := mergeTransferRecordsAll(records, []int{0, 1, 2})
	if len(changes) != 2 || imported != 2 || skipped != 1 {
		t.Fatalf("unexpected mixed merge counts: changes=%d imported=%d skipped=%d", len(changes), imported, skipped)
	}
}

func TestMergeTransferRecordsSkipsMissingTokenAndBadIndex(t *testing.T) {
	records := []transferRecord{{UID: "uid", Domain: "copilot.tencent.com"}}
	changes, imported, skipped := mergeTransferRecords(records, []int{0, 9}, mustSite("workbuddy"))
	if len(changes) != 0 || imported != 0 || skipped != 2 {
		t.Fatalf("unexpected skip counts: changes=%d imported=%d skipped=%d", len(changes), imported, skipped)
	}
}

func mustSite(id string) site {
	got, ok := selectSite(id)
	if !ok {
		panic("unknown test site: " + id)
	}
	return got
}
