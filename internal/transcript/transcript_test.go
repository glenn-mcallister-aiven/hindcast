package transcript

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFile(t *testing.T) {
	path := filepath.Join("testdata", "sample.jsonl")
	turns, err := ParseFile(path, time.Time{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}

	t1 := turns[0]
	if t1.PromptText != "add retry logic to fetcher" {
		t.Errorf("turn1 prompt = %q", t1.PromptText)
	}
	if t1.Model != "claude-opus-4-7" {
		t.Errorf("turn1 model = %q", t1.Model)
	}
	// Tool calls: 1 Read + 1 Edit + 1 Bash = 3 distinct tools.
	if got := t1.ToolCalls["Read"]; got != 1 {
		t.Errorf("turn1 Read = %d", got)
	}
	if got := t1.ToolCalls["Edit"]; got != 1 {
		t.Errorf("turn1 Edit = %d", got)
	}
	if got := t1.ToolCalls["Bash"]; got != 1 {
		t.Errorf("turn1 Bash = %d", got)
	}
	// Files: fetcher.go is touched by Read and Edit — should dedupe to 1.
	if t1.FilesTouched != 1 {
		t.Errorf("turn1 files_touched = %d, want 1", t1.FilesTouched)
	}
	// Wall: 10:00:00 → 10:05:06 = 306s.
	if w := t1.WallSeconds(); w != 306 {
		t.Errorf("turn1 wall = %d, want 306", w)
	}
	// Active: gaps between tool_uses, clamped at 60s each.
	//   tu1 10:00:04 (first, no gap)
	//   tu2 10:00:10 gap 6s
	//   tu3 10:05:00 gap 290s clamped to 60s
	// Total active = 0 + 6 + 60 = 66s.
	if a := t1.ActiveSeconds; a != 66 {
		t.Errorf("turn1 active = %d, want 66 (6 + 60 clamp)", a)
	}

	t2 := turns[1]
	if t2.PromptText != "fix the test flake in auth" {
		t.Errorf("turn2 prompt = %q", t2.PromptText)
	}
	if t2.FilesTouched != 1 {
		t.Errorf("turn2 files_touched = %d", t2.FilesTouched)
	}
	if w := t2.WallSeconds(); w != 45 {
		t.Errorf("turn2 wall = %d, want 45", w)
	}
	// Subagent entry between turns must NOT contaminate turn2 counts.
	for name := range t2.ToolCalls {
		if name == "SubagentTask" {
			t.Errorf("subagent tool leaked into turn2: %v", t2.ToolCalls)
		}
	}
}

func TestParseFileSince(t *testing.T) {
	path := filepath.Join("testdata", "sample.jsonl")
	cutoff, _ := time.Parse(time.RFC3339, "2026-04-19T10:09:00Z")
	turns, err := ParseFile(path, cutoff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("want 1 turn after cutoff, got %d", len(turns))
	}
	if turns[0].PromptText != "fix the test flake in auth" {
		t.Errorf("filtered turn prompt = %q", turns[0].PromptText)
	}
}

// CC writes one JSONL entry per content block of a single assistant
// response (thinking / text / tool_use), each repeating the SAME message.id
// and usage.output_tokens. OutputTokens must count each response's usage
// once, not once per block — else it inflates ~2-2.5x.
func TestParseTokenCostDedupesUsagePerMessageID(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-06-06T10:00:00Z","message":{"role":"user","content":"do it"}}`,
		// One response, three split entries, same id "m1", usage repeated.
		`{"type":"assistant","timestamp":"2026-06-06T10:00:05Z","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"thinking"}],"usage":{"output_tokens":164}}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:00:06Z","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"x1","name":"Bash","input":{}}],"usage":{"output_tokens":164}}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:00:07Z","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"x2","name":"Bash","input":{}}],"usage":{"output_tokens":164}}}`,
		// A genuinely separate second response.
		`{"type":"assistant","timestamp":"2026-06-06T10:00:10Z","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"output_tokens":40}}}`,
	}
	turns, err := Parse(strings.NewReader(strings.Join(lines, "\n")), time.Time{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if turns[0].OutputTokens != 204 {
		t.Errorf("OutputTokens = %d, want 204 (164 once + 40), not per-block inflation", turns[0].OutputTokens)
	}
}

func TestParseTokenCost(t *testing.T) {
	// One turn that spawns two subagents. Main-session output_tokens come
	// from assistant usage; subagent fan-out cost comes from the
	// <usage>subagent_tokens:N</usage> footers in the tool_result blocks
	// (which arrive as non-new-prompt "user" messages).
	lines := []string{
		`{"type":"user","timestamp":"2026-06-06T10:00:00Z","message":{"role":"user","content":"migrate the tests"}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:00:05Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"t1","name":"Task","input":{}}],"usage":{"output_tokens":120}}}`,
		`{"type":"user","timestamp":"2026-06-06T10:01:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"done (you cannot continue this agent)\n<usage>subagent_tokens: 36768\ntool_uses: 6\nduration_ms: 3442</usage>"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:01:05Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"t2","name":"Task","input":{}}],"usage":{"output_tokens":80}}}`,
		`{"type":"user","timestamp":"2026-06-06T10:02:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"ok\n<usage>subagent_tokens: 21776\ntool_uses: 11\nduration_ms: 9000</usage>"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:02:10Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"all migrated"}],"usage":{"output_tokens":40}}}`,
	}
	turns, err := Parse(strings.NewReader(strings.Join(lines, "\n")), time.Time{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	tr := turns[0]
	if tr.OutputTokens != 240 {
		t.Errorf("OutputTokens = %d, want 240 (120+80+40)", tr.OutputTokens)
	}
	if tr.SubagentTokens != 58544 {
		t.Errorf("SubagentTokens = %d, want 58544 (36768+21776)", tr.SubagentTokens)
	}
	if tr.AgentCount != 2 {
		t.Errorf("AgentCount = %d, want 2", tr.AgentCount)
	}
	if got := tr.TokenCost(); got != 58784 {
		t.Errorf("TokenCost() = %d, want 58784", got)
	}
}

// A Bash/Read tool_result that echoes transcript text — even a full,
// well-formed `<usage>subagent_tokens:` footer (e.g. from `cat`-ing a
// .jsonl) — must NOT count, because its originating tool is not a subagent
// spawner. This is the over-count that poisoned transcript-reading sessions
// before footers were attributed to their tool_use.
func TestParseTokenCostIgnoresEchoedText(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-06-06T10:00:00Z","message":{"role":"user","content":"how many subagent_tokens are in the log?"}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:00:05Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{}}],"usage":{"output_tokens":50}}}`,
		// Bash result echoing a genuine-looking footer from a transcript dump.
		`{"type":"user","timestamp":"2026-06-06T10:00:30Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","content":[{"type":"text","text":"line: <usage>subagent_tokens: 99999\ntool_uses: 5\nduration_ms: 1000</usage>"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:00:40Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"counted"}],"usage":{"output_tokens":20}}}`,
	}
	turns, err := Parse(strings.NewReader(strings.Join(lines, "\n")), time.Time{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].SubagentTokens != 0 {
		t.Errorf("SubagentTokens = %d, want 0 (Bash echo must not count)", turns[0].SubagentTokens)
	}
	if turns[0].AgentCount != 0 {
		t.Errorf("AgentCount = %d, want 0", turns[0].AgentCount)
	}
}

// A real subagent (Agent/Task) whose own narrative QUOTES a footer must
// count only the trailing CC-appended footer, not the quoted one.
func TestParseTokenCostTakesLastFooterPerResult(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-06-06T10:00:00Z","message":{"role":"user","content":"review the diff"}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:00:05Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"a1","name":"Agent","input":{}}],"usage":{"output_tokens":30}}}`,
		// Agent result: narrative quotes a footer (11111), then CC appends the real one (44444).
		`{"type":"user","timestamp":"2026-06-06T10:01:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a1","content":[{"type":"text","text":"I found <usage>subagent_tokens: 11111</usage> in the log.\n\n<usage>subagent_tokens: 44444\ntool_uses: 9\nduration_ms: 5000</usage>"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-06-06T10:01:05Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"done"}],"usage":{"output_tokens":10}}}`,
	}
	turns, err := Parse(strings.NewReader(strings.Join(lines, "\n")), time.Time{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if turns[0].SubagentTokens != 44444 {
		t.Errorf("SubagentTokens = %d, want 44444 (trailing footer only, not the quoted 11111)", turns[0].SubagentTokens)
	}
	if turns[0].AgentCount != 1 {
		t.Errorf("AgentCount = %d, want 1", turns[0].AgentCount)
	}
}

func TestParseTail(t *testing.T) {
	path := filepath.Join("testdata", "sample.jsonl")
	// Small tailBytes → only the tail of the file is read; earlier turn
	// should be dropped because its user-prompt line is not in the tail.
	turns, err := ParseTail(path, 512, time.Time{})
	if err != nil {
		t.Fatalf("parse tail: %v", err)
	}
	// With 512 bytes of tail, we may land partway through turn2 events.
	// Behaviour we assert: no panic, no crash, and any returned turns are
	// valid (have a non-empty prompt).
	for _, tr := range turns {
		if tr.PromptText == "" {
			t.Errorf("tail returned turn with empty prompt: %+v", tr)
		}
	}
}
