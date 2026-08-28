package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	root := t.TempDir()
	records := filepath.Join(root, "records")
	if err := os.Mkdir(records, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "codex-cli 0.149.1"
  exit 0
fi
if [ "$1" = "login" ] && [ "$2" = "status" ]; then
  echo "Logged in using ChatGPT"
  exit 0
fi
pid=$$
printf '%s\n' "$@" >"$RECORD_DIR/args.$pid"
schema=""
previous=""
for argument in "$@"; do
  if [ "$previous" = "--output-schema" ]; then schema="$argument"; fi
  previous="$argument"
done
if [ -n "$schema" ]; then cp "$schema" "$RECORD_DIR/schema.$pid"; fi
prompt=$(dd bs=65536 2>/dev/null)
printf '%s' "$prompt" >"$RECORD_DIR/stdin.$pid"
case "$prompt" in
  block)
    printf '%s\n' "$pid" >"$RECORD_DIR/pid.$pid"
    while :; do :; done
    ;;
  malformed)
    echo '{oops'
    ;;
  fail)
    echo '{"type":"turn.failed","error":{"message":"fake failure"}}'
    ;;
  exit)
    echo 'fake stderr' >&2
    exit 7
    ;;
  *)
    printf '{"type":"item.completed","item":{"type":"error","message":"diagnostic"}}\n'
    printf '{"type":"item.completed","item":{"type":"agent_message","text":"answer:%s"}}\n' "$prompt"
    printf '{"type":"turn.completed","usage":{"input_tokens":4,"cached_input_tokens":1,"output_tokens":3,"reasoning_output_tokens":1}}\n'
    ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECORD_DIR", records)
	t.Setenv("CODEX_HOME", filepath.Join(root, "home", "codex"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	skill := filepath.Join(os.Getenv("CODEX_HOME"), "skills", "fake", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: fake\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner, err := NewRunner(context.Background(), binary, log)
	if err != nil {
		t.Fatal(err)
	}
	return runner, records
}

func readSingleRecord(t *testing.T, directory, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(directory, prefix+"*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 1 {
			file, err := os.Open(matches[0])
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var contents strings.Builder
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				contents.WriteString(scanner.Text())
				contents.WriteByte('\n')
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			return strings.TrimSuffix(contents.String(), "\n")
		}
		if len(matches) > 1 {
			t.Fatalf("multiple %s records: %v", prefix, matches)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s record in %s", prefix, directory)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }
