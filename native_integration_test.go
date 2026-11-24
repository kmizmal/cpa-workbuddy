package workbuddy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestNativeABI crosses the public native seam in a separate C process. No CPA
// internal Go package, generated cgo header, or Go-level fake is used by probe.c.
func TestNativeABI(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	for _, directory := range []string{"test-tmp", "cache/go-build", "tmp"} {
		if err := os.MkdirAll(filepath.Join(out, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stage, err := os.MkdirTemp(filepath.Join(out, "test-tmp"), "native-abi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(stage); err != nil {
			t.Errorf("remove native test artifacts: %v", err)
		}
	})
	environment := nativeEnvironment(out)
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Fatalf("independent native probe requires gcc: %v", err)
	}
	library := os.Getenv("WORKBUDDY_TEST_LIBRARY")
	if library == "" {
		goCommand, err := exec.LookPath("go")
		if err != nil {
			t.Fatalf("building native plugin requires go: %v", err)
		}
		extension := ".so"
		switch runtime.GOOS {
		case "windows":
			extension = ".dll"
		case "darwin":
			extension = ".dylib"
		}
		library = filepath.Join(stage, "cpa-workbuddy"+extension)
		runNativeCommand(t, root, environment, goCommand, "build", "-buildmode=c-shared", "-o", library, "./cmd/cpa-workbuddy")
	} else {
		library, err = filepath.Abs(library)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(library)
		if err != nil {
			t.Fatalf("WORKBUDDY_TEST_LIBRARY: %v", err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("WORKBUDDY_TEST_LIBRARY must name a regular shared library: %s", library)
		}
	}
	probe := filepath.Join(stage, "abi-probe")
	if runtime.GOOS == "windows" {
		probe += ".exe"
	}
	arguments := []string{"-std=c11", "-Wall", "-Wextra", "-Werror", "-O2", "-o", probe, filepath.Join(root, "testdata", "abi", "probe.c")}
	if runtime.GOOS == "linux" || runtime.GOOS == "freebsd" {
		arguments = append(arguments, "-ldl")
	}
	if runtime.GOOS != "windows" {
		arguments = append(arguments, "-pthread")
	}
	runNativeCommand(t, root, environment, gcc, arguments...)
	rawVersion, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	expectedVersion := strings.TrimSpace(string(rawVersion))
	output := runNativeCommand(t, root, environment, probe, library, expectedVersion)

	// The C reader checks native wire semantics; Go independently validates the
	// entire JSON encoding, including schema 6's base64-encoded response Body.
	scanner := bufio.NewScanner(bytes.NewReader(output))
	seen := map[string]bool{}
	for scanner.Scan() {
		label, raw, found := strings.Cut(scanner.Text(), "\t")
		if !found || seen[label] {
			t.Fatalf("invalid or duplicate probe record: %s", scanner.Text())
		}
		seen[label] = true
		var envelope struct {
			OK     bool
			Result json.RawMessage
			Error  *struct{ Code, Message string }
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("%s: invalid JSON: %v", label, err)
		}
		switch label {
		case "registration":
			var result struct {
				SchemaVersion int `json:"schema_version"`
				Metadata      struct {
					Name, Version, Author, GitHubRepository string
					ConfigFields                            []struct{ Name, Type string }
				}
				Capabilities map[string]json.RawMessage
			}
			if err := json.Unmarshal(envelope.Result, &result); err != nil {
				t.Fatal(err)
			}
			if !envelope.OK || envelope.Error != nil || result.SchemaVersion != 10 || result.Metadata.Name != "cpa-workbuddy" || result.Metadata.Version != expectedVersion || result.Metadata.Author != "sheepyu" || result.Metadata.GitHubRepository != "https://github.com/ygq-future/cpa-workbuddy" || len(result.Metadata.ConfigFields) != 4 || len(result.Capabilities) != 8 || string(result.Capabilities["management_api"]) != "true" || string(result.Capabilities["quota_provider"]) != "true" || string(result.Capabilities["auth_provider"]) != "true" || string(result.Capabilities["model_provider"]) != "true" || string(result.Capabilities["executor"]) != "true" || string(result.Capabilities["executor_model_scope"]) != `"oauth"` || string(result.Capabilities["executor_input_formats"]) != `["openai","openai-response","claude","gemini"]` || string(result.Capabilities["executor_output_formats"]) != `["openai","openai-response","claude","gemini"]` {
				t.Fatalf("registration violates CPA contract: %s", raw)
			}
		case "routes":
			var result struct {
				Routes    []struct{ Method, Path string }
				Resources []struct{ Path, Menu, Description string }
			}
			if err := json.Unmarshal(envelope.Result, &result); err != nil {
				t.Fatal(err)
			}
			if !envelope.OK || len(result.Routes) != 13 || result.Routes[0].Method != "GET" || result.Routes[0].Path != "/v0/management/plugins/cpa-workbuddy/status" || len(result.Resources) != 1 || result.Resources[0].Path != "/panel" {
				t.Fatalf("unexpected management registration: %s", raw)
			}
			transferRoutes := map[string]bool{}
			for _, route := range result.Routes {
				if route.Method == "POST" {
					transferRoutes[route.Path] = true
				}
			}
			for _, path := range []string{"preview", "export", "import"} {
				want := "/v0/management/plugins/cpa-workbuddy/transfer/" + path
				if !transferRoutes[want] {
					t.Fatalf("management.register is missing %s: %s", want, raw)
				}
			}
		case "status":
			var result struct {
				StatusCode int
				Headers    map[string][]string
				Body       []byte
			}
			if err := json.Unmarshal(envelope.Result, &result); err != nil {
				t.Fatal(err)
			}
			var status struct {
				Name, Version string
				ABIVersion    int `json:"abi_version"`
				SchemaVersion int `json:"schema_version"`
			}
			if err := json.Unmarshal(result.Body, &status); err != nil {
				t.Fatalf("invalid status JSON body: %v", err)
			}
			if !envelope.OK || result.StatusCode != 200 || len(result.Headers["Content-Type"]) != 1 || result.Headers["Content-Type"][0] != "application/json; charset=utf-8" || status.Name != "cpa-workbuddy" || status.Version != expectedVersion || status.ABIVersion != 1 || status.SchemaVersion != 10 {
				t.Fatalf("unexpected native status response: %s", raw)
			}
		case "auth-identifier":
			var result struct{ Identifier string }
			if err := json.Unmarshal(envelope.Result, &result); err != nil {
				t.Fatal(err)
			}
			if !envelope.OK || envelope.Error != nil || result.Identifier != "workbuddy" {
				t.Fatalf("unexpected native auth identity: %s", raw)
			}
		case "host-roundtrip", "host-shutdown":
			var result struct{ Callbacks, Releases int }
			if err := json.Unmarshal(envelope.Result, &result); err != nil {
				t.Fatal(err)
			}
			want := 8
			if label == "host-shutdown" {
				want = 9
			}
			if !envelope.OK || envelope.Error != nil || result.Callbacks != want || result.Releases != want {
				t.Fatalf("unexpected native host ownership summary: %s", raw)
			}
		default:
			want, expected := nativeErrorScenarios[label]
			if !expected || envelope.OK || envelope.Error == nil || envelope.Error.Code != want || envelope.Error.Message == "" {
				t.Fatalf("unexpected negative scenario %s: %s", label, raw)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(nativeErrorScenarios)+6 {
		t.Fatalf("native probe omitted scenarios: got %d, want %d; output:\n%s", len(seen), len(nativeErrorScenarios)+6, output)
	}
	for label := range nativeErrorScenarios {
		if !seen[label] {
			t.Errorf("missing native negative scenario: %s", label)
		}
	}
}

var nativeErrorScenarios = map[string]string{
	"nil-method":                   "invalid_method",
	"nil-positive-length":          "invalid_request",
	"oversize":                     "invalid_request",
	"c-int-overflow":               "invalid_request",
	"nil-request":                  "invalid_request",
	"before-registration":          "not_registered",
	"malformed-json":               "invalid_request",
	"embedded-nul":                 "invalid_request",
	"schema-mismatch":              "incompatible_schema",
	"unknown-method":               "unknown_method",
	"no-plugin-identifier":         "unknown_method",
	"auth-before-registration":     "not_registered",
	"executor-before-registration": "not_registered",
	"wrong-path":                   "route_not_found",
	"wrong-http-method":            "method_not_allowed",
	"quiesced":                     "plugin_quiesced",
	"rpc-shutdown":                 "plugin_shutdown",
	"native-shutdown":              "plugin_shutdown",
	"direct-native-shutdown":       "plugin_shutdown",
}

func nativeEnvironment(out string) []string {
	settings := map[string]string{
		"CGO_ENABLED": "1",
		"GOTOOLCHAIN": "local",
		"GOENV":       "off",
		"GOCACHE":     filepath.Join(out, "cache", "go-build"),
		"GOTMPDIR":    filepath.Join(out, "tmp"),
		"TMPDIR":      filepath.Join(out, "tmp"),
		"TMP":         filepath.Join(out, "tmp"),
		"TEMP":        filepath.Join(out, "tmp"),
	}
	environment := make([]string, 0, len(os.Environ())+len(settings))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := settings[strings.ToUpper(key)]; !replaced {
			environment = append(environment, entry)
		}
	}
	for key, value := range settings {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func runNativeCommand(t *testing.T, root string, environment []string, command string, arguments ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	process := exec.CommandContext(ctx, command, arguments...)
	process.Dir = root
	process.Env = environment
	output, err := process.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %q: %v\n%s", command, arguments, err, output)
	}
	return output
}
