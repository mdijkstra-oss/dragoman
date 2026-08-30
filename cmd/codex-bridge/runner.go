package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"gopkg.in/yaml.v3"
)

const supportedVersion = "codex-cli 0.149.1"
const killedChildPipeGrace = 5 * time.Second

var disabledFeatures = []string{
	"apps", "artifact", "browser_use", "browser_use_external", "browser_use_full_cdp_access",
	"code_mode", "computer_use", "default_mode_request_user_input", "enable_mcp_apps", "goals",
	"hooks", "image_generation", "in_app_browser", "multi_agent", "multi_agent_v2", "plugins",
	"recommended_plugins", "remote_plugin", "request_permissions_tool", "shell_tool", "skill_mcp_dependency_install",
	"skill_search", "standalone_web_search", "tool_call_mcp_elicitation", "tool_suggest", "unified_exec", "view_image",
	"workspace_dependencies",
}

type Runner struct {
	Path string
	// Codex's own web search runs inside the CLI and comes back as text.
	// Off by default; on, the standalone search tool stays enabled too.
	Web     bool
	workDir string
	env     []string
	skills  []skillConfig
	log     *slog.Logger
}

type Result struct {
	Answer      string
	Usage       *base.Usage
	Diagnostics []string
	seenAnswer  bool
}

type skillConfig struct {
	Path string
	Name string
}

func NewRunner(ctx context.Context, binary string, log *slog.Logger) (*Runner, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("%q not found on PATH; install Codex CLI %s or pass --binary", binary, supportedVersion)
	}
	env := childEnv(os.Environ())
	if err := checkVersion(ctx, path, env); err != nil {
		return nil, err
	}
	if err := checkLogin(ctx, path, env); err != nil {
		return nil, err
	}
	workDir, err := os.MkdirTemp("", "codex-bridge-work-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(workDir, 0o555); err != nil {
		_ = os.RemoveAll(workDir)
		return nil, err
	}
	skills, err := discoverSkills(os.Getenv("CODEX_HOME"), os.Getenv("HOME"))
	if err != nil {
		_ = os.Chmod(workDir, 0o755)
		_ = os.RemoveAll(workDir)
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{Path: path, workDir: workDir, env: env, skills: skills, log: log}, nil
}

func (r *Runner) Close() {
	_ = os.Chmod(r.workDir, 0o755)
	_ = os.RemoveAll(r.workDir)
}

func checkVersion(ctx context.Context, path string, env []string) error {
	command := exec.CommandContext(ctx, path, "--version")
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("checking %s version: %w", path, err)
	}
	got := strings.TrimSpace(string(output))
	if got != supportedVersion {
		return fmt.Errorf("unsupported Codex CLI version %q; codex-bridge requires %s", got, supportedVersion)
	}
	return nil
}

func checkLogin(ctx context.Context, path string, env []string) error {
	command := exec.CommandContext(ctx, path, "login", "status")
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Logged in using ChatGPT") {
		return errors.New(`not authenticated with ChatGPT; run "codex login"`)
	}
	return nil
}

func childEnv(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "CONDUCTOR_") || (strings.HasPrefix(name, "CODEX_") && name != "CODEX_HOME") {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func discoverSkills(codexHome, home string) ([]skillConfig, error) {
	if codexHome == "" && home != "" {
		codexHome = filepath.Join(home, ".codex")
	}
	roots := []string{}
	if codexHome != "" {
		roots = append(roots, filepath.Join(codexHome, "skills"))
	}
	if home != "" {
		roots = append(roots, filepath.Join(home, ".agents", "skills"))
	}
	found := map[string]skillConfig{}
	visited := map[string]bool{}
	var walk func(string) error
	walk = func(root string) error {
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		canonicalRoot, err = filepath.Abs(canonicalRoot)
		if err != nil {
			return err
		}
		if visited[canonicalRoot] {
			return nil
		}
		visited[canonicalRoot] = true
		return filepath.WalkDir(canonicalRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				info, err := os.Stat(path)
				if err != nil {
					return err
				}
				if info.IsDir() {
					return walk(path)
				}
			}
			if !entry.IsDir() && entry.Name() == "SKILL.md" {
				absolute, err := filepath.Abs(path)
				if err != nil {
					return err
				}
				name, err := readSkillName(absolute)
				if err != nil {
					return err
				}
				found[absolute] = skillConfig{Path: absolute, Name: name}
			}
			return nil
		})
	}
	for _, root := range roots {
		err := walk(root)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("discovering skills under %s: %w", root, err)
		}
	}
	skills := make([]skillConfig, 0, len(found))
	for _, skill := range found {
		skills = append(skills, skill)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Path < skills[j].Path })
	return skills, nil
}

func readSkillName(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	parts := bytes.SplitN(contents, []byte("---"), 3)
	if len(parts) < 3 || len(bytes.TrimSpace(parts[0])) != 0 {
		return "", fmt.Errorf("skill %s has no YAML frontmatter", path)
	}
	var metadata struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(parts[1], &metadata); err != nil {
		return "", fmt.Errorf("reading skill name from %s: %w", path, err)
	}
	if metadata.Name == "" {
		return "", fmt.Errorf("skill %s has no name", path)
	}
	return metadata.Name, nil
}

func (r *Runner) args(call Invocation, schemaPath string) []string {
	args := []string{
		"exec", "--json", "--ephemeral", "--strict-config", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "-C", r.workDir, "--sandbox", "read-only",
		"-c", `approval_policy="never"`, "-c", `model_provider="openai"`,
		"-c", "developer_instructions=" + tomlString(call.Instructions),
		"-c", `personality="none"`, "-c", "include_permissions_instructions=false",
		"-c", "include_apps_instructions=false", "-c", "include_collaboration_mode_instructions=false",
		"-c", "include_environment_context=false",
		"-c", `tools.experimental_request_user_input={enabled=false}`,
		"-c", `tools.update_plan={enabled=false}`,
	}
	if r.Web {
		args = append(args, "-c", `web_search="live"`, "-c", "tools.web_search=true")
	} else {
		args = append(args, "-c", `web_search="disabled"`, "-c", "tools.web_search=false")
	}
	if len(r.skills) > 0 {
		var config strings.Builder
		config.WriteString("skills.config=[")
		for index, skill := range r.skills {
			if index > 0 {
				config.WriteByte(',')
			}
			// Codex 0.149.1 accepts a path key but does not apply that
			// override. Its skill registry is keyed by the frontmatter name.
			config.WriteString("{name=")
			config.WriteString(tomlString(skill.Name))
			config.WriteString(",enabled=false}")
		}
		config.WriteByte(']')
		args = append(args, "-c", config.String())
	}
	for _, feature := range disabledFeatures {
		if r.Web && feature == "standalone_web_search" {
			continue
		}
		args = append(args, "--disable", feature)
	}
	if call.Model != "" {
		args = append(args, "--model", call.Model)
	}
	if call.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(call.Effort))
	}
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	return append(args, "-")
}

func (r *Runner) Run(ctx context.Context, call Invocation) (Result, error) {
	schemaPath := ""
	if len(call.Schema) > 0 {
		file, err := os.CreateTemp("", "codex-bridge-schema-*.json")
		if err != nil {
			return Result{}, err
		}
		schemaPath = file.Name()
		defer os.Remove(schemaPath)
		if _, err := file.Write(call.Schema); err != nil {
			_ = file.Close()
			return Result{}, err
		}
		if err := file.Close(); err != nil {
			return Result{}, err
		}
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	command := exec.CommandContext(ctx, r.Path, r.args(call, schemaPath)...)
	command.Dir = r.workDir
	command.Env = r.env
	command.Stdin = strings.NewReader(call.Prompt)
	command.WaitDelay = killedChildPipeGrace
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("reading from %s: %w", r.Path, err)
	}
	if err := command.Start(); err != nil {
		return Result{}, fmt.Errorf("starting %s: %w", r.Path, err)
	}
	result, readErr := readFrames(stdout, r.log)
	if readErr != nil {
		stop()
	}
	waitErr := command.Wait()
	if readErr != nil {
		return result, readErr
	}
	if waitErr != nil {
		return result, fmt.Errorf("%s: %w%s", r.Path, waitErr, detail(stderr))
	}
	return result, nil
}

type execFrame struct {
	Type string `json:"type"`
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Message string `json:"message"`
	} `json:"item"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		InputTokens           int `json:"input_tokens"`
		CachedInputTokens     int `json:"cached_input_tokens"`
		CacheWriteInputTokens int `json:"cache_write_input_tokens"`
		OutputTokens          int `json:"output_tokens"`
		ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	} `json:"usage"`
}

func readFrames(reader io.Reader, log *slog.Logger) (Result, error) {
	result := Result{}
	completed := false
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame execFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return result, fmt.Errorf("malformed codex exec frame: %w", err)
		}
		if log != nil {
			log.Debug("frame", "type", frame.Type, "item_type", frame.Item.Type)
		}
		switch frame.Type {
		case "item.completed":
			switch frame.Item.Type {
			case "agent_message":
				result.Answer = frame.Item.Text
				result.seenAnswer = true
			case "error":
				if message := cleanDiagnostic(frame.Item.Message); message != "" {
					result.Diagnostics = append(result.Diagnostics, message)
				}
			}
		case "turn.completed":
			completed = true
			result.Usage = base.CountUsage(frame.Usage.InputTokens, frame.Usage.OutputTokens, frame.Usage.ReasoningOutputTokens, frame.Usage.CachedInputTokens, frame.Usage.CacheWriteInputTokens)
		case "turn.failed":
			message := cleanDiagnostic(frame.Error.Message)
			if message == "" {
				message = "the Codex CLI reported turn.failed"
			}
			return result, errors.New(message)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("reading the Codex CLI stream: %w", err)
	}
	if !completed {
		return result, errors.New("the Codex CLI ended without turn.completed")
	}
	if !result.seenAnswer {
		return result, errors.New("the Codex CLI completed without an agent_message")
	}
	return result, nil
}

func detail(diagnostics *bytes.Buffer) string {
	text := strings.TrimSpace(diagnostics.String())
	if text == "" {
		return ""
	}
	return ": " + text
}
