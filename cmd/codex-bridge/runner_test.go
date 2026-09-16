package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Codex exec loads local skills before it handles the prompt, so the bridge
// must discover and disable them rather than merely forwarding request fields.
// Symlinks matter because skill installers commonly use them.
func TestDiscoverSkills(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	home := filepath.Join(root, "home")
	paths := []string{
		filepath.Join(codexHome, "skills", "one", "SKILL.md"),
		filepath.Join(home, ".agents", "skills", "two", "nested", "SKILL.md"),
	}
	for index, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		contents := "---\nname: skill-" + strconv.Itoa(index) + "\n---\n"
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "linked-skill")
	linkedPath := filepath.Join(target, "SKILL.md")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linkedPath, []byte("---\nname: linked\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".agents", "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	found, err := discoverSkills(codexHome, home)
	if err != nil {
		t.Fatal(err)
	}
	want := []skillConfig{
		{Path: paths[0], Name: "skill-0"},
		{Path: paths[1], Name: "skill-1"},
		{Path: linkedPath, Name: "linked"},
	}
	for index := range want {
		canonical, err := filepath.EvalSymlinks(want[index].Path)
		if err != nil {
			t.Fatal(err)
		}
		want[index].Path = canonical
	}
	slices.SortFunc(want, func(a, b skillConfig) int { return strings.Compare(a.Path, b.Path) })
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("skills = %+v, want %+v", found, want)
	}
}

func TestChildEnvDropsCredentialsAndParentContext(t *testing.T) {
	got := childEnv([]string{
		"PATH=/bin", "HOME=/home", "OPENAI_API_KEY=secret", "OPENAI_BASE_URL=x",
		"CODEX_HOME=/codex", "CODEX_THREAD_ID=parent", "CODEX_API_KEY=secret",
		"CONDUCTOR_WORKSPACE_ID=x", "OTHER=value",
	})
	want := "PATH=/bin HOME=/home CODEX_HOME=/codex OTHER=value"
	if strings.Join(got, " ") != want {
		t.Fatalf("environment = %v, want %s", got, want)
	}
}

func TestStartupRequiresPinnedVersionAndChatGPTLogin(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "$FAKE_VERSION"; exit 0; fi
if [ "$1" = "login" ] && [ "$2" = "status" ]; then echo "$FAKE_LOGIN"; exit 0; fi
exit 2
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	good := []string{"PATH=/bin", "FAKE_VERSION=" + supportedVersion, "FAKE_LOGIN=Logged in using ChatGPT"}
	if err := checkVersion(ctx, binary, good); err != nil {
		t.Fatal(err)
	}
	current := []string{"PATH=/bin", "FAKE_VERSION=codex-cli 0.154.0", "FAKE_LOGIN=Logged in using ChatGPT"}
	if err := checkVersion(ctx, binary, current); err != nil {
		t.Fatal(err)
	}
	if err := checkLogin(ctx, binary, good); err != nil {
		t.Fatal(err)
	}
	badVersion := []string{"PATH=/bin", "FAKE_VERSION=codex-cli 0.150.0"}
	if err := checkVersion(ctx, binary, badVersion); err == nil || !strings.Contains(err.Error(), supportedVersion) {
		t.Fatalf("wrong-version error = %v", err)
	}
	badLogin := []string{"PATH=/bin", "FAKE_LOGIN=Logged in using an API key"}
	if err := checkLogin(ctx, binary, badLogin); err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("wrong-login error = %v", err)
	}
}

func TestReadFrames(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread"}`,
		`{"type":"item.completed","item":{"type":"error","message":"warning only"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"draft"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":3,"cache_write_input_tokens":4,"output_tokens":8,"reasoning_output_tokens":2}}`,
	}, "\n")
	result, err := readFrames(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "answer" || len(result.Diagnostics) != 1 || result.Diagnostics[0] != "warning only" {
		t.Fatalf("result: %+v", result)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 8 || result.Usage.TotalTokens != 18 || result.Usage.InputTokenDetails.CachedTokens != 3 || result.Usage.OutputTokenDetails.ReasoningTokens != 2 {
		t.Fatalf("usage: %+v", result.Usage)
	}
}

func TestReadFramesRejectsProtocolFailures(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		want   string
	}{
		{name: "malformed", stream: `{oops`, want: "malformed"},
		{name: "failed", stream: `{"type":"turn.failed","error":{"message":"denied"}}`, want: "denied"},
		{name: "missing completion", stream: `{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`, want: "turn.completed"},
		{name: "missing answer", stream: `{"type":"turn.completed","usage":{}}`, want: "agent_message"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := readFrames(strings.NewReader(testCase.stream), nil)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestRunnerArgumentsAreIsolated(t *testing.T) {
	runner, records := fakeRunner(t)
	defer runner.Close()
	call := Invocation{Model: "gpt-5.4", Instructions: "be brief", Effort: "high", Prompt: "hello", Schema: json.RawMessage(`{"type":"object"}`)}
	if _, err := runner.Run(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	args := readSingleRecord(t, records, "args.")
	for _, required := range []string{
		"exec", "--json", "--ephemeral", "--strict-config", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "read-only", `approval_policy="never"`, `model_provider="openai"`,
		`developer_instructions="be brief"`, `personality="none"`, "include_permissions_instructions=false",
		"tools.web_search=false", `tools.update_plan={enabled=false}`, "--output-schema", "--model", "gpt-5.4",
		`model_reasoning_effort="high"`, "--disable", "shell_tool", "multi_agent", "plugins", "skill_search",
		`skills.config=[{name="fake",enabled=false}]`,
	} {
		if !strings.Contains(args, required) {
			t.Errorf("arguments do not contain %q:\n%s", required, args)
		}
	}
	if strings.Contains(args, "standalone_web_search") == false || !strings.Contains(args, `web_search="disabled"`) {
		t.Errorf("web search is not disabled by default:\n%s", args)
	}
	if stdin := readSingleRecord(t, records, "stdin."); stdin != "hello" {
		t.Fatalf("stdin = %q", stdin)
	}
	if schema := readSingleRecord(t, records, "schema."); schema != `{"type":"object"}` {
		t.Fatalf("schema = %s", schema)
	}
	info, err := os.Stat(runner.workDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("work directory mode = %o", info.Mode().Perm())
	}
}

func TestRunnerCancellationReapsChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process assertion")
	}
	runner, records := fakeRunner(t)
	defer runner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := runner.Run(ctx, Invocation{Model: "gpt-5.4", Instructions: defaultInstructions, Prompt: "block"})
	if err == nil {
		t.Fatal("cancelled invocation succeeded")
	}
	pidText := readSingleRecord(t, records, "pid.")
	pid, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("child %d is still running", pid)
	}
}

func TestConcurrentRequestsRemainIndependent(t *testing.T) {
	runner, _ := fakeRunner(t)
	defer runner.Close()
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, prompt := range []string{"first", "second"} {
		prompt := prompt
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := runner.Run(context.Background(), Invocation{Model: "gpt-5.4", Instructions: defaultInstructions, Prompt: prompt})
			if err != nil {
				errors <- err
				return
			}
			if result.Answer != "answer:"+prompt {
				errors <- &testError{"answer = " + result.Answer}
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestWebFlagLeavesCodexSearchOn(t *testing.T) {
	runner, records := fakeRunner(t)
	defer runner.Close()
	runner.Web = true
	if _, err := runner.Run(context.Background(), Invocation{Model: "gpt-5.4", Prompt: "hello"}); err != nil {
		t.Fatal(err)
	}
	args := readSingleRecord(t, records, "args.")
	for _, required := range []string{`web_search="live"`, "tools.web_search=true"} {
		if !strings.Contains(args, required) {
			t.Errorf("arguments do not contain %q:\n%s", required, args)
		}
	}
	for _, forbidden := range []string{"standalone_web_search", `web_search="disabled"`} {
		if strings.Contains(args, forbidden) {
			t.Errorf("arguments still contain %q:\n%s", forbidden, args)
		}
	}
}
