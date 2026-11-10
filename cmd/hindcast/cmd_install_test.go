package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMergeRemovesLegacyStatusline verifies that mergeClaudeSettings
// (the install path) cleans up pre-v0.6 hindcast statusLine entries.
// Without this migration, existing users upgrading to v0.6 would see
// "unknown command: statusline" on every prompt because the subcommand
// was removed.
func TestMergeRemovesLegacyStatusline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	// Pre-v0.6 settings: hindcast statusline wired in.
	existing := map[string]any{
		"statusLine": map[string]any{
			"type":    "command",
			"command": "/usr/local/bin/hindcast statusline",
		},
		"hooks": map[string]any{},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := mergeClaudeSettings("/usr/local/bin/hindcast"); err != nil {
		t.Fatalf("merge: %v", err)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["statusLine"]; present {
		t.Errorf("legacy hindcast statusLine should be removed, got:\n%s", got)
	}
}

// TestMergePreservesUserStatusline ensures the migration is conservative:
// only delete the entry if it's a hindcast statusline command. A user
// with their own statusline (or one from another tool) must be preserved.
func TestMergePreservesUserStatusline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	existing := map[string]any{
		"statusLine": map[string]any{
			"type":    "command",
			"command": "/usr/local/bin/my-status-bar",
		},
		"hooks": map[string]any{},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := mergeClaudeSettings("/usr/local/bin/hindcast"); err != nil {
		t.Fatalf("merge: %v", err)
	}

	got, _ := os.ReadFile(settingsPath)
	var out map[string]any
	_ = json.Unmarshal(got, &out)
	sl, ok := out["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("user statusLine should be preserved, got:\n%s", got)
	}
	if cmd, _ := sl["command"].(string); cmd != "/usr/local/bin/my-status-bar" {
		t.Errorf("user statusLine command mutated: %q", cmd)
	}
}

// TestUnmergeRemovesLegacyStatusline mirrors the install-time migration
// for the uninstall path so a user uninstalling can clean up cruft from
// any version.
func TestUnmergeRemovesLegacyStatusline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	existing := map[string]any{
		"statusLine": map[string]any{
			"type":    "command",
			"command": "/usr/local/bin/hindcast statusline",
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := unmergeClaudeSettings(); err != nil {
		t.Fatalf("unmerge: %v", err)
	}

	got, _ := os.ReadFile(settingsPath)
	var out map[string]any
	_ = json.Unmarshal(got, &out)
	if _, present := out["statusLine"]; present {
		t.Errorf("legacy statusLine should be removed by uninstall, got:\n%s", got)
	}
}

func TestIsLegacyHindcastStatusline(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"/usr/local/bin/hindcast statusline", true},
		{"/home/user/go/bin/hindcast statusline", true},
		{"hindcast statusline", true},
		{"hindcast pending", false},
		{"hindcast record", false},
		{"my-status-bar", false},
		{"/usr/local/bin/somethingelse statusline", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLegacyHindcastStatusline(tc.cmd); got != tc.want {
			t.Errorf("isLegacyHindcastStatusline(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestMergeDropsStaleMCPEntry: pre-fix installs wrote mcpServers.hindcast
// into settings.json, which Claude Code ignores. Install must remove it
// while leaving other servers alone.
func TestMergeDropsStaleMCPEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		t.Fatal(err)
	}
	existing := `{"mcpServers":{"hindcast":{"command":"/x/hindcast","args":["mcp"]},"other":{"command":"o"}}}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := mergeClaudeSettings("/usr/local/bin/hindcast"); err != nil {
		t.Fatalf("merge: %v", err)
	}

	got, _ := os.ReadFile(settingsPath)
	var out map[string]any
	_ = json.Unmarshal(got, &out)
	mcps, _ := out["mcpServers"].(map[string]any)
	if _, present := mcps["hindcast"]; present {
		t.Errorf("stale mcpServers.hindcast should be removed, got:\n%s", got)
	}
	if _, present := mcps["other"]; !present {
		t.Errorf("unrelated MCP server should be preserved, got:\n%s", got)
	}
}

// TestRegisterMCPUsesClaudeCLI puts a fake `claude` on PATH that logs its
// argv, and checks install/uninstall drive `claude mcp` at user scope.
func TestRegisterMCPUsesClaudeCLI(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	registerMCP("/usr/local/bin/hindcast")
	unregisterMCP()

	got, _ := os.ReadFile(log)
	want := "mcp remove --scope user hindcast\n" +
		"mcp add --scope user hindcast -- /usr/local/bin/hindcast mcp\n" +
		"mcp remove --scope user hindcast\n"
	if string(got) != want {
		t.Errorf("claude calls:\n%s\nwant:\n%s", got, want)
	}
}
