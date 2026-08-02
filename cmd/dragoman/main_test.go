package main_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binary builds the command once so the exit codes can be observed the way a
// hook or a CI step observes them.
func binary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dragoman")
	build := exec.Command("go", "build", "-o", path, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return path
}

// TestExitCodes covers the reason two is reserved for "the tool could not
// run": that is the only thing that makes it trustworthy in a hook or a CI
// step. kong's own failure path exits one, which is reserved here for the
// backend saying no, so every usage failure has to be converted.
func TestExitCodes(t *testing.T) {
	command := binary(t)
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  int
	}{
		{name: "help", args: []string{"--help"}, want: 0},
		{name: "bad flag", args: []string{"--not-a-flag"}, want: 2},
		{name: "unknown protocol", args: []string{"--protocol", "anthropic-messages", "--backend", "openai"}, want: 2},
		{name: "unknown backend protocol", args: []string{"--backend-protocol", "nope", "--endpoint", "http://x"}, stdin: `{}`, want: 2},
		{name: "unknown preset", args: []string{"--backend", "nope"}, stdin: `{}`, want: 2},
		{name: "no backend", args: []string{}, stdin: `{}`, want: 2},
		{name: "unparseable input", args: []string{"--backend", "openai", "--no-execute"}, stdin: `not json`, want: 2},
		{name: "missing credential", args: []string{"--backend", "openai"}, stdin: `{"input":[]}`, want: 2},
		{
			name:  "strict drop",
			args:  []string{"--backend", "anthropic", "--strict", "--no-execute"},
			stdin: `{"input":[],"temperature":0.7}`,
			want:  2,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := exec.Command(command, testCase.args...)
			run.Stdin = strings.NewReader(testCase.stdin)
			run.Env = append(os.Environ(), "OPENAI_API_KEY=", "ANTHROPIC_API_KEY=")
			output, _ := run.CombinedOutput()
			code := run.ProcessState.ExitCode()
			if code != testCase.want {
				t.Errorf("exit = %d, want %d\n%s", code, testCase.want, output)
			}
		})
	}
}

// TestNoExecuteNeedsNoCredential keeps the diagnostic mode usable without a
// key, which is what makes it the mode most tests run in.
func TestNoExecuteNeedsNoCredential(t *testing.T) {
	command := binary(t)
	run := exec.Command(command, "--backend", "gemini", "--no-execute")
	run.Stdin = strings.NewReader(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	run.Env = append(os.Environ(), "GEMINI_API_KEY=")
	output, err := run.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(output), `"user_input"`) {
		t.Errorf("translation missing: %s", output)
	}
}

// TestHealthcheck covers the one exit code a container runtime reads. Zero is the
// only value that means healthy and one is the only value that has ever meant
// unhealthy, so the usage code dragoman reserves for its own failures must not
// escape through this command.
func TestHealthcheck(t *testing.T) {
	command := binary(t)
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer serving.Close()

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer refusing.Close()

	silent := httptest.NewServer(http.NotFoundHandler())
	unreachable := address(silent.URL)
	silent.Close()

	cases := []struct {
		name string
		addr string
		want int
	}{
		{name: "a serving listener is healthy", addr: address(serving.URL), want: 0},
		{name: "a listener answering anything else is unhealthy", addr: address(refusing.URL), want: 1},
		{name: "nothing listening is unhealthy", addr: unreachable, want: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			run := exec.Command(command, "healthcheck", "--addr", test.addr)
			run.Stderr = &strings.Builder{}
			if got := exitCodeOf(run.Run()); got != test.want {
				t.Errorf("exit code = %d, want %d", got, test.want)
			}
		})
	}
}

func address(url string) string { return strings.TrimPrefix(url, "http://") }

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
