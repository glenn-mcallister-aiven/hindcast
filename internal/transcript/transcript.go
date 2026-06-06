// Package transcript parses Claude Code session transcripts
// (~/.claude/projects/*/*.jsonl) into per-turn records.
//
// Each JSONL line is one CC event: a user message, an assistant message,
// or a streamed assistant content block. We group events into turns
// bounded by top-level user prompts, accumulating tool-call counts,
// files touched, active-time gaps, and duration.
//
// Subagent events (isSidechain: true) are excluded — hindcast scores the
// main session only. Tool-result "user" messages are also excluded from
// turn boundaries; only new-prompt user messages start a turn.
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Turn is one recorded UserPromptSubmit → Stop span. Claude's
// self-reported estimate is NOT parsed from response text — it's
// written by the hindcast_estimate MCP tool and read separately by
// the Stop hook.
type Turn struct {
	PromptTS      time.Time
	StopTS        time.Time
	PromptText    string // raw prompt — ephemeral, never persisted
	ResponseChars int
	ResponseText  string // ephemeral assistant text, capped — feeds the outcome judge only; never persisted
	ToolCalls     map[string]int
	FilesTouched  int
	Model         string
	ActiveSeconds int

	// Token cost (v2). OutputTokens is the main session's generated
	// tokens (summed assistant usage.output_tokens — NOT input/cache,
	// which re-count the whole context every message and would balloon).
	// SubagentTokens sums the `<usage>subagent_tokens: N` footers that
	// every completed subagent (Task/Agent or Workflow) reports back into
	// the parent transcript's tool_result text — the fan-out cost the
	// main session is otherwise blind to. AgentCount is how many such
	// completions this turn saw. TokenCost() is the headline metric.
	OutputTokens   int
	SubagentTokens int
	AgentCount     int
}

// TokenCost is the headline token metric for a turn: generative work in
// the main session plus the rolled-up cost of any subagent fan-out. Cache
// reads and re-counted input tokens are deliberately excluded.
func (t Turn) TokenCost() int { return t.OutputTokens + t.SubagentTokens }

// WallSeconds is a convenience derived from PromptTS and StopTS.
func (t Turn) WallSeconds() int {
	if t.PromptTS.IsZero() || t.StopTS.IsZero() || t.StopTS.Before(t.PromptTS) {
		return 0
	}
	return int(t.StopTS.Sub(t.PromptTS).Seconds())
}

// activeGapCap clamps any inter-tool gap above this to this value —
// approximates "Claude was idle / user was AFK or approving a prompt".
const activeGapCap = 60 * time.Second

// responseTextCap bounds the ephemeral assistant text captured per turn
// for the outcome judge. ~6KB is enough for the judge to tell whether
// the turn landed without ballooning the judge prompt (and its cost) on
// long responses. The leading text usually carries the "what I did"
// summary, so a head-truncation loses little signal.
const responseTextCap = 6000

type rawEntry struct {
	ParentUUID  *string         `json:"parentUuid"`
	IsSidechain bool            `json:"isSidechain"`
	Type        string          `json:"type"`
	Message     json.RawMessage `json:"message"`
	Timestamp   time.Time       `json:"timestamp"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *usage          `json:"usage,omitempty"`
}

type usage struct {
	OutputTokens int `json:"output_tokens"`
}

// subagentTokensRe matches the `<usage>subagent_tokens: N` footer Claude
// Code appends to a completed subagent's tool_result (verified identical
// for Task/Agent and Workflow subagents).
//
// Attribution, not just matching, is what keeps this honest. A bare regex
// over all tool_result text over-counts badly: any Bash/Read/Grep that
// echoes transcript bytes — or a subagent narrative that quotes a footer —
// looks identical to a real footer (measured 18-of-20 footers false on this
// project's own dev sessions, which read transcripts). So scanning is gated
// two ways in Parse: (1) only tool_result blocks whose originating tool_use
// is a subagent spawner (isSubagentTool) are scanned, which drops the
// Bash/Read echoes entirely; (2) within such a block only the LAST footer
// counts — CC appends exactly one, trailing, so an earlier match is the
// subagent quoting a footer in its own output. The `<usage>` anchor is
// retained as defense-in-depth.
var subagentTokensRe = regexp.MustCompile(`<usage>\s*subagent_tokens:\s*(\d+)`)

// isSubagentTool reports whether a tool_use of this name spawns a subagent
// that reports a token footer back to the parent. Bash/Read/Grep/etc. do
// not — and their results are exactly what echoes footer text.
func isSubagentTool(name string) bool {
	switch name {
	case "Task", "Agent", "Workflow":
		return true
	}
	return false
}

// lastSubagentFooter returns the value of the LAST <usage>subagent_tokens
// footer in text (CC appends it at the very end of a subagent result; any
// earlier match is quoted narrative). ok is false when there is none.
func lastSubagentFooter(text string) (int, bool) {
	ms := subagentTokensRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return 0, false
	}
	v, err := strconv.Atoi(ms[len(ms)-1][1])
	if err != nil {
		return 0, false
	}
	return v, true
}

// toolResultText extracts the text payload of a tool_result block's content,
// which CC encodes as either a bare string or an array of text blocks.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []contentBlock
	if json.Unmarshal(raw, &arr) == nil {
		var b strings.Builder
		for _, blk := range arr {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use id
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result → its tool_use
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result nested payload
}

// Parse reads a transcript from r and returns all turns whose PromptTS
// is >= since. Pass time.Time{} to get every turn.
func Parse(r io.Reader, since time.Time) ([]Turn, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)

	var turns []Turn
	var curr *Turn
	var lastTool time.Time
	filePaths := map[string]bool{}
	// tool_use ids that spawned a subagent — populated as assistant
	// tool_use blocks stream past, consumed when their tool_result arrives.
	// Lets us count footers only from real subagent results, not from
	// Bash/Read output that happens to echo transcript text.
	subagentIDs := map[string]bool{}
	// Assistant message ids whose usage we've already counted. CC writes one
	// JSONL entry per content block (thinking / text / tool_use) and repeats
	// the SAME message.id and usage.output_tokens on each — so a naive
	// per-entry sum inflates OutputTokens ~2-2.5x. Count usage once per id.
	countedUsage := map[string]bool{}

	finalize := func() {
		if curr == nil {
			return
		}
		if !curr.PromptTS.Before(since) {
			curr.FilesTouched = len(filePaths)
			turns = append(turns, *curr)
		}
	}

	for sc.Scan() {
		var e rawEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.IsSidechain {
			continue
		}
		var m rawMessage
		if err := json.Unmarshal(e.Message, &m); err != nil {
			continue
		}

		switch e.Type {
		case "user":
			if !isNewUserPrompt(m.Content) {
				// Tool-result injection (not a turn boundary). Harvest the
				// subagent fan-out cost reported back to the parent — but
				// ONLY from tool_results whose tool_use was a subagent
				// spawner, and only the trailing footer of each. See
				// subagentTokensRe for why both gates matter.
				if curr != nil {
					for _, b := range extractBlocks(m.Content) {
						if b.Type != "tool_result" || !subagentIDs[b.ToolUseID] {
							continue
						}
						if v, ok := lastSubagentFooter(toolResultText(b.Content)); ok {
							curr.SubagentTokens += v
							curr.AgentCount++
						}
					}
				}
				continue
			}
			finalize()
			curr = &Turn{
				PromptTS:   e.Timestamp,
				PromptText: extractUserString(m.Content),
				ToolCalls:  map[string]int{},
			}
			filePaths = map[string]bool{}
			lastTool = time.Time{}

		case "assistant":
			if curr == nil {
				continue
			}
			curr.StopTS = e.Timestamp
			if m.Model != "" && curr.Model == "" {
				curr.Model = m.Model
			}
			if m.Usage != nil && (m.ID == "" || !countedUsage[m.ID]) {
				curr.OutputTokens += m.Usage.OutputTokens
				if m.ID != "" {
					countedUsage[m.ID] = true
				}
			}
			blocks := extractBlocks(m.Content)
			for _, b := range blocks {
				switch b.Type {
				case "text":
					curr.ResponseChars += len(b.Text)
					// Capture text up to a cap for the outcome judge.
					// Ephemeral — dropped when the Turn is consumed; never
					// written to any record.
					if n := responseTextCap - len(curr.ResponseText); n > 0 {
						if len(b.Text) <= n {
							curr.ResponseText += b.Text
						} else {
							// Trim n back to a UTF-8 rune boundary so the cap
							// can't split a multibyte rune mid-sequence.
							for n > 0 && !utf8.RuneStart(b.Text[n]) {
								n--
							}
							curr.ResponseText += b.Text[:n]
						}
					}
				case "tool_use":
					curr.ToolCalls[b.Name]++
					if isSubagentTool(b.Name) && b.ID != "" {
						subagentIDs[b.ID] = true
					}
					if !lastTool.IsZero() {
						gap := e.Timestamp.Sub(lastTool)
						if gap > activeGapCap {
							gap = activeGapCap
						}
						if gap > 0 {
							curr.ActiveSeconds += int(gap.Seconds())
						}
					}
					lastTool = e.Timestamp
					if p := extractFilePath(b.Input); p != "" {
						filePaths[p] = true
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return turns, err
	}
	finalize()
	return turns, nil
}

// ParseFile is Parse over an os.Open'd path.
func ParseFile(path string, since time.Time) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, since)
}

// ParseTail reads only the last tailBytes of path and parses turns whose
// PromptTS is >= since. The tail-seek trades completeness for speed on
// large transcripts — a turn whose user message is before the tail
// window will be dropped. Caller uses `since` to narrow to their turn.
func ParseTail(path string, tailBytes int64, since time.Time) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var start int64
	if stat.Size() > tailBytes {
		start = stat.Size() - tailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	// If we're not at the start of the file, the first line is probably
	// partial — discard it.
	r := bufio.NewReader(f)
	if start > 0 {
		if _, err := r.ReadBytes('\n'); err != nil && err != io.EOF {
			return nil, err
		}
	}
	return Parse(r, since)
}

// isNewUserPrompt returns true when the content is a bare string (the
// shape CC emits for a fresh user prompt) as opposed to the array form
// it uses for tool_result injections.
func isNewUserPrompt(raw json.RawMessage) bool {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	return strings.HasPrefix(trimmed, `"`)
}

func extractUserString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Array form fallback — concatenate any text blocks.
	blocks := extractBlocks(raw)
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func extractBlocks(raw json.RawMessage) []contentBlock {
	// Try array form first.
	var arr []contentBlock
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	// String form → single text block.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []contentBlock{{Type: "text", Text: s}}
	}
	return nil
}

// maxEffectiveContextChars bounds how much prior conversational text
// feeds into classification/BM25 at UserPromptSubmit. ~2500 chars is
// roughly 500 tokens or 2 conversational turns on average — enough to
// capture "what's the task" without diluting signal with drift.
const maxEffectiveContextChars = 2500

// ComposeEffectiveTask returns the current user prompt prefixed with
// the most recent ~maxEffectiveContextChars of prior conversational
// text (user prompts + assistant text blocks, in chronological order).
// This is the "unit of work" hindcast classifies — so "do it" after a
// long plan gets classified on the plan's content, not the literal
// two-word prompt.
//
// Budget-based rather than message-count-based: assistant turns can
// have 1-10 text blocks depending on tool interleaving, which makes
// a fixed-N-entries window inconsistent. A char budget scales with
// available content uniformly.
//
// If the transcript is missing or has no prior content, returns the
// userPrompt alone.
func ComposeEffectiveTask(userPrompt, transcriptPath string) string {
	prior := readRecentTextsBudget(transcriptPath, maxEffectiveContextChars)
	if prior == "" {
		return userPrompt
	}
	return prior + "\n\n" + userPrompt
}

// readRecentTextsBudget tail-reads the transcript and returns the most
// recent text content fitting within budget chars. Walks forward
// through the tail window collecting text-bearing entries, then
// trims from the front to fit the budget (most-recent content wins
// when we have to cut). Joined with "\n\n" between entries.
func readRecentTextsBudget(path string, budget int) string {
	if path == "" || budget <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return ""
	}
	var start int64
	const tailBytes = 512 * 1024
	if stat.Size() > tailBytes {
		start = stat.Size() - tailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}

	r := bufio.NewReader(f)
	if start > 0 {
		if _, err := r.ReadBytes('\n'); err != nil && err != io.EOF {
			return ""
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var texts []string
	for sc.Scan() {
		var e rawEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.IsSidechain {
			continue
		}
		var m rawMessage
		if err := json.Unmarshal(e.Message, &m); err != nil {
			continue
		}
		switch e.Type {
		case "user":
			if isNewUserPrompt(m.Content) {
				if t := strings.TrimSpace(extractUserString(m.Content)); t != "" {
					texts = append(texts, t)
				}
			}
		case "assistant":
			blocks := extractBlocks(m.Content)
			for _, b := range blocks {
				if b.Type == "text" {
					if t := strings.TrimSpace(b.Text); t != "" {
						texts = append(texts, t)
					}
				}
			}
		}
	}

	// Walk backward from newest text, accumulating until we hit budget.
	// Most-recent content gets included first; older content drops off
	// when we run out of budget.
	remaining := budget
	var selected []string
	for i := len(texts) - 1; i >= 0; i-- {
		t := texts[i]
		if len(t) >= remaining {
			// Tail-cap: take the final `remaining` chars of this entry
			// so we capture the most recent content within the budget.
			// Align to a UTF-8 rune boundary so we don't produce
			// fragments like \x94 at the start (continuation bytes
			// from a sliced multi-byte character).
			cut := len(t) - remaining
			for cut < len(t) && !utf8.RuneStart(t[cut]) {
				cut++
			}
			t = t[cut:]
			selected = append([]string{t}, selected...)
			break
		}
		selected = append([]string{t}, selected...)
		remaining -= len(t) + 2 // account for "\n\n" joiner
		if remaining <= 0 {
			break
		}
	}
	return strings.Join(selected, "\n\n")
}

func extractFilePath(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if fp, ok := m["file_path"].(string); ok && fp != "" {
		return fp
	}
	if np, ok := m["notebook_path"].(string); ok && np != "" {
		return np
	}
	return ""
}
