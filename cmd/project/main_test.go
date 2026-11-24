package main_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	repositoryRoot   string
	controllerBinary string
)

func TestMain(m *testing.M) {
	os.Exit(runControllerTests(m))
}

func runControllerTests(m *testing.M) (status int) {
	root, err := findRepositoryRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	repositoryRoot = root
	if err := configureTestEnvironment(filepath.Join(root, "out")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	proxyPath := filepath.ToSlash(filepath.Join(root, "out", "cache", "go-mod", "cache", "download"))
	if filepath.VolumeName(root) != "" {
		proxyPath = "/" + proxyPath
	}
	if err := os.Setenv("GOPROXY", (&url.URL{Scheme: "file", Path: proxyPath}).String()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	stage, err := os.MkdirTemp(filepath.Join(root, "out", "tmp"), "project-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := removeFixtureAll(stage); err != nil {
			fmt.Fprintln(os.Stderr, err)
			status = 1
		}
	}()
	controllerBinary = filepath.Join(stage, "project")
	if runtime.GOOS == "windows" {
		controllerBinary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", controllerBinary, ".")
	command.Dir = filepath.Join(root, "cmd", "project")
	if output, err := command.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build project CLI: %v\n%s", err, output)
		return 1
	}
	return m.Run()
}

func findRepositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		module, moduleErr := os.Stat(filepath.Join(directory, "go.mod"))
		version, versionErr := os.Stat(filepath.Join(directory, "VERSION"))
		if moduleErr == nil && versionErr == nil && module.Mode().IsRegular() && version.Mode().IsRegular() {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("cannot find repository root containing go.mod and VERSION")
		}
		directory = parent
	}
}

func configureTestEnvironment(out string) error {
	locations := map[string]string{
		"GOCACHE":             filepath.Join(out, "cache", "go-build"),
		"GOMODCACHE":          filepath.Join(out, "cache", "go-mod"),
		"GOPATH":              filepath.Join(out, "cache", "gopath"),
		"GOBIN":               filepath.Join(out, "tools", "bin"),
		"GOTMPDIR":            filepath.Join(out, "tmp"),
		"TMPDIR":              filepath.Join(out, "tmp"),
		"TMP":                 filepath.Join(out, "tmp"),
		"TEMP":                filepath.Join(out, "tmp"),
		"GOLANGCI_LINT_CACHE": filepath.Join(out, "cache", "golangci-lint"),
		"GOPLSCACHE":          filepath.Join(out, "cache"),
		"XDG_CACHE_HOME":      filepath.Join(out, "cache", "xdg"),
		"XDG_CONFIG_HOME":     filepath.Join(out, "config"),
		"XDG_DATA_HOME":       filepath.Join(out, "data"),
		"GOTELEMETRYDIR":      filepath.Join(out, "telemetry"),
	}
	for key, directory := range locations {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
		if err := os.Setenv(key, directory); err != nil {
			return err
		}
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			if err := os.Unsetenv(key); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"GOOS", "GOARCH", "GODEBUG", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "RELEASE_TAG", "WORKBUDDY_TEST_LIBRARY"} {
		if err := os.Unsetenv(key); err != nil {
			return err
		}
	}
	for key, value := range map[string]string{
		"CGO_ENABLED":         "1",
		"GOENV":               "off",
		"GOFLAGS":             "-mod=readonly -modcacherw",
		"GOTOOLCHAIN":         "local",
		"GOWORK":              "off",
		"GOTELEMETRY":         "off",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
	} {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

type fixture struct {
	root string
}

type commandResult struct {
	stdout string
	stderr string
	code   int
}

// removeFixtureAll deletes a temporary tree, retrying the transient sharing
// violation Windows reports while an exited process releases its cwd handle.
func removeFixtureAll(path string) error {
	var err error
	for range 10 {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root, err := os.MkdirTemp(filepath.Join(repositoryRoot, "out", "tmp"), "project-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeFixtureAll(root); err != nil {
			t.Error(err)
		}
	})
	f := fixture{root: root}
	for _, relative := range []string{"go.mod", "go.sum", "VERSION"} {
		copyFixtureFile(t, filepath.Join(repositoryRoot, relative), filepath.Join(root, relative))
	}
	return f
}

func (f fixture) execute(t *testing.T, arguments ...string) commandResult {
	t.Helper()
	return f.run(t, controllerBinary, arguments...)
}

func (f fixture) run(t *testing.T, executable string, arguments ...string) commandResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = f.root
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %q: %v\n%s%s", executable, arguments, ctx.Err(), stdout.String(), stderr.String())
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%s %q: %v", executable, arguments, err)
		}
		code = exit.ExitCode()
	}
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func requireSuccess(t *testing.T, result commandResult) {
	t.Helper()
	if result.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", result.code, result.stdout, result.stderr)
	}
}

func requireFailure(t *testing.T, result commandResult, diagnostic string) {
	t.Helper()
	if result.code == 0 || !strings.Contains(result.stderr, diagnostic) {
		t.Fatalf("want failure containing %q; exit %d\nstdout:\n%s\nstderr:\n%s", diagnostic, result.code, result.stdout, result.stderr)
	}
}

func writeFixtureFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func copyFixtureFile(t *testing.T, source, destination string) {
	t.Helper()
	writeFixtureFile(t, destination, string(readFixtureFile(t, source)))
}

func copyFixtureTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("fixture source is not a regular file: %s", path)
		}
		copyFixtureFile(t, path, target)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func copyFixtureChecksumState(t *testing.T, root string) {
	t.Helper()
	for _, relative := range []string{
		filepath.Join("out", "cache", "go-mod", "cache", "download", "sumdb"),
		filepath.Join("out", "cache", "gopath", "pkg", "sumdb"),
	} {
		copyFixtureTree(t, filepath.Join(repositoryRoot, relative), filepath.Join(root, relative))
	}
}

func TestCommitMessageFile(t *testing.T) {
	f := newFixture(t)
	message := filepath.Join(f.root, "message")
	for _, test := range []struct {
		name       string
		message    string
		diagnostic string
	}{
		{name: "body", message: "feat(native)!: expose status\n\nBREAKING CHANGE: require schema 6\n"},
		{name: "BOM", message: "\ufefffix: expose status\n"},
		{name: "CRLF", message: "fix: expose status\r\n\r\nbody\r\n"},
		{name: "bad subject", message: "ship it\n", diagnostic: "invalid Conventional Commit"},
		{name: "empty", diagnostic: "commit message is empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeFixtureFile(t, message, test.message)
			result := f.execute(t, "commit-msg", "--message-file", message)
			if test.diagnostic != "" {
				requireFailure(t, result, test.diagnostic)
				return
			}
			requireSuccess(t, result)
			if result.stdout != "" || result.stderr != "" {
				t.Fatalf("valid message produced output: %q %q", result.stdout, result.stderr)
			}
		})
	}
}

func TestCommitBodyRequiresBlankSeparator(t *testing.T) {
	f := newFixture(t)
	message := filepath.Join(f.root, "message")
	writeFixtureFile(t, message, "fix: expose status\nbody without separator\n")
	requireFailure(t, f.execute(t, "commit-msg", "--message-file", message), "blank line")
	writeFixtureFile(t, message, "fix: expose status\n\nbody with separator\n")
	requireSuccess(t, f.execute(t, "commit-msg", "--message-file", message))
}

func (f fixture) git(t *testing.T, arguments ...string) {
	t.Helper()
	result := f.run(t, "git", arguments...)
	if result.code != 0 {
		t.Fatalf("git %q: exit %d\nstdout:\n%s\nstderr:\n%s", arguments, result.code, result.stdout, result.stderr)
	}
}

func (f fixture) initRepository(t *testing.T) {
	t.Helper()
	f.git(t, "init", "-b", "main")
	f.git(t, "config", "user.name", "WorkBuddy Test")
	f.git(t, "config", "user.email", "workbuddy@example.com")
	f.git(t, "commit", "--allow-empty", "-m", "chore: initial commit")
}

// markPublished points the branch's upstream at a tracking ref without
// requiring a remote, so the pending-commit gate sees a published base.
func (f fixture) markPublished(t *testing.T) {
	t.Helper()
	f.git(t, "config", "remote.origin.url", f.root)
	f.git(t, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	f.git(t, "config", "branch.main.remote", "origin")
	f.git(t, "config", "branch.main.merge", "refs/heads/main")
	f.git(t, "update-ref", "refs/remotes/origin/main", "HEAD")
}

func TestQualityValidatesPendingCommitMessages(t *testing.T) {
	f := newFixture(t)
	writeFixtureFile(t, filepath.Join(f.root, "version.go"), "//go:embed VERSION\n")
	f.initRepository(t)
	f.markPublished(t)
	requireSuccess(t, f.execute(t, "quality", "--stage", "version"))

	f.git(t, "commit", "--allow-empty", "-m", "ship the panel")
	requireFailure(t, f.execute(t, "quality", "--stage", "version"), "invalid Conventional Commit subject")

	f.git(t, "commit", "--amend", "--allow-empty", "-m", "fix: ship the panel")
	requireSuccess(t, f.execute(t, "quality", "--stage", "version"))
}

func TestQualityIgnoresCommitsOfEnclosingRepository(t *testing.T) {
	parent := newFixture(t)
	writeFixtureFile(t, filepath.Join(parent.root, "version.go"), "//go:embed VERSION\n")
	parent.initRepository(t)
	parent.markPublished(t)
	parent.git(t, "commit", "--allow-empty", "-m", "ship the panel")

	nested := fixture{root: filepath.Join(parent.root, "nested")}
	for _, relative := range []string{"go.mod", "VERSION", "version.go"} {
		copyFixtureFile(t, filepath.Join(parent.root, relative), filepath.Join(nested.root, relative))
	}
	requireSuccess(t, nested.execute(t, "quality", "--stage", "version"))
}

func TestVersionTagMismatchPreservesSource(t *testing.T) {
	f := newFixture(t)
	version := filepath.Join(f.root, "VERSION")
	writeFixtureFile(t, version, "0.1.0\n")
	writeFixtureFile(t, filepath.Join(f.root, "version.go"), "//go:embed VERSION\n")
	requireFailure(t, f.execute(t, "version", "--tag", "v0.1.1"), "must equal v0.1.0")
	if got := string(readFixtureFile(t, version)); got != "0.1.0\n" {
		t.Fatalf("VERSION changed: %q", got)
	}
	requireSuccess(t, f.execute(t, "version", "--tag", "v0.1.0"))
}

func TestNativeSmokeRequiresABITest(t *testing.T) {
	f := newFixture(t)
	copyFixtureChecksumState(t, f.root)
	// Deliberately omit native_integration_test.go and this CLI test package.
	for _, relative := range []string{
		"version.go",
		"cmd/cpa-workbuddy/main.go",
		"cmd/cpa-workbuddy/native.c",
		"cmd/cpa-workbuddy/native.h",
	} {
		copyFixtureFile(t, filepath.Join(repositoryRoot, relative), filepath.Join(f.root, relative))
	}
	err := filepath.WalkDir(filepath.Join(repositoryRoot, "internal"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), ".html")) || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(repositoryRoot, path)
		if err != nil {
			return err
		}
		copyFixtureFile(t, path, filepath.Join(f.root, relative))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(f.root, "ordinary_test.go"), `package workbuddy_test

import "testing"

func TestNativeHelper(t *testing.T) { t.Log("ordinary helper") }
`)
	result := f.execute(t, "quality", "--stage", "test")
	requireFailure(t, result, "native smoke test did not execute")
	if strings.Contains(result.stdout, "quality passed") {
		t.Fatalf("missing ABI test reported success: %s", result.stdout)
	}
}

func TestGeneratedGoSourcesStayOutsideModule(t *testing.T) {
	f := newFixture(t)
	writeFixtureFile(t, filepath.Join(f.root, "sample.go"), "package sample\n")
	writeFixtureFile(t, filepath.Join(f.root, "out", "cache", "legacy", "generated.go"), "package generated\nvar Value = missingSymbol\n")
	result := f.execute(t, "quality", "--stage", "vet")
	if result.code != 0 {
		t.Fatalf("artifact sources entered the root Go scan: exit %d\n%s\n%s", result.code, result.stdout, result.stderr)
	}
}
