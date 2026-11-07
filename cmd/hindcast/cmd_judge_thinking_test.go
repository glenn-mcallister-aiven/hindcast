package main

import "testing"

func TestDisableThinkingOnlyForHaiku5(t *testing.T) {
	m := map[string]any{}
	disableThinking(m, "claude-haiku-5-5")
	th, ok := m["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("haiku-5-5 request must carry thinking disabled, got %v", m)
	}
	m = map[string]any{}
	disableThinking(m, "claude-haiku-4-5")
	if _, ok := m["thinking"]; ok {
		t.Fatalf("haiku-4-5 request must not carry a thinking field, got %v", m)
	}
}
