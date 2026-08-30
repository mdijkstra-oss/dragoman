package main

import (
	"strings"
	"testing"
)

func TestWebFlagNamesTheCLITools(t *testing.T) {
	runner := &Runner{}
	if args := strings.Join(runner.args(), " "); !strings.HasSuffix(args, "--tools ") {
		t.Errorf("tools are not empty by default: %s", args)
	}
	runner.Web = true
	if args := strings.Join(runner.args(), " "); !strings.HasSuffix(args, "--tools WebSearch,WebFetch --allowedTools WebSearch,WebFetch") || !strings.Contains(args, "--safe-mode") {
		t.Errorf("web tools are not named: %s", args)
	}
}
