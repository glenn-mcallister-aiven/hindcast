package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/justinstimatze/hindcast/internal/hook"
	"github.com/justinstimatze/hindcast/internal/judge"
	"github.com/justinstimatze/hindcast/internal/store"
)

// defaultJudgeModel is the grader. Haiku: the judgment is coarse
// (3 classes) and runs once per turn off the hot path, so the cheapest
// capable model is the right call. Override with HINDCAST_JUDGE_MODEL.
//
// Haiku 5.5 is $0.10/$0.50 per MTok (Haiku 4.5 was $1/$5). It thinks by default and
// rejects thinking under a forced tool_choice, so the request disables it (see disableThinking).
const defaultJudgeModel = "claude-haiku-5-5"

// judgeHTTPTimeout bounds a single judge call. The judge runs inside the
// Stop worker (30s budget) — keep well under it so a slow API call can't
// starve the rest of record finalization. Best-effort: on timeout we
// just don't label.
const judgeHTTPTimeout = 20 * time.Second

// judgeEnabled reports whether the outcome judge should run. Opt-in: the
// default build is network-free, matching the predictor's no-LLM
// guarantee. Set HINDCAST_JUDGE=1 to turn it on.
func judgeEnabled() bool {
	return os.Getenv("HINDCAST_JUDGE") == "1"
}

// judgeCachedFile is the on-disk shape of a cached verdict.
type judgeCachedFile struct {
	Verdict string `json:"verdict"`
	Model   string `json:"model"`
	TS      string `json:"ts"`
}

// judgeTurn grades one completed turn. Returns (verdict, model, true) on
// success. Fail-open: any problem — judge disabled, no API key, cache
// miss + network error, malformed response — returns ok=false and the
// turn simply goes un-judged. Never blocks or errors the record path.
//
// Redaction is enforced HERE, not at call sites: prompt/response are
// scrubbed of secrets before they're hashed for the cache or sent to the
// API, so no egress path can forget to redact and the cache only ever
// holds redacted-derived keys. Caches by content hash: a given turn hits
// the API at most once, ever (see store.JudgeCacheDir). The cache key
// folds in model + rubric version + rubric text, so a model/rubric
// change re-judges rather than serving a stale verdict.
func judgeTurn(prompt, response, toolsSummary string) (string, string, bool) {
	if !judgeEnabled() {
		return "", "", false
	}
	if prompt == "" {
		return "", "", false
	}
	// Scrub secrets before anything leaves this function (cache key or API).
	prompt = judge.Redact(prompt)
	response = judge.Redact(response)
	model := os.Getenv("HINDCAST_JUDGE_MODEL")
	if model == "" {
		model = defaultJudgeModel
	}

	key := judge.CacheKey(model, prompt, response, toolsSummary)
	cacheDir, err := store.JudgeCacheDir()
	if err != nil {
		return "", "", false
	}
	cachePath := filepath.Join(cacheDir, key+".json")
	if data, err := os.ReadFile(cachePath); err == nil {
		var c judgeCachedFile
		if json.Unmarshal(data, &c) == nil && judge.Valid(c.Verdict) {
			return c.Verdict, c.Model, true
		}
	}

	apiKey := loadAPIKey()
	if apiKey == "" {
		hook.Logf("judge", "no API key (shell env / cwd .env / ~/.config/hindcast/.env); skipping")
		return "", "", false
	}

	verdict, ok := callJudgeAPI(apiKey, model, prompt, response, toolsSummary)
	if !ok {
		return "", "", false
	}

	// Best-effort cache write — a failed write just means we re-judge
	// this turn next time, which is harmless (and rare).
	if data, err := json.Marshal(judgeCachedFile{
		Verdict: verdict,
		Model:   model,
		TS:      time.Now().UTC().Format(time.RFC3339),
	}); err == nil {
		_ = os.WriteFile(cachePath, data, 0600)
	}
	return verdict, model, true
}

// disableThinking adds thinking:{type:disabled} for models that think by default (Haiku 5.x).
// A forced tool_choice cannot run with thinking on; Haiku 4.5 needs no such field.
func disableThinking(reqBody map[string]any, model string) {
	if strings.Contains(model, "haiku-5") {
		reqBody["thinking"] = map[string]any{"type": "disabled"}
	}
}

// callJudgeAPI makes the single forced-tool call to the Messages API and
// returns the verdict. Mirrors queryEstimate's request shape (same
// headers, same forced-tool pattern). Single attempt — the judge is
// best-effort and runs on every Stop, so retry/backoff isn't worth the
// added worker latency; a dropped label is fine.
func callJudgeAPI(apiKey, model, prompt, response, toolsSummary string) (string, bool) {
	reqBody := map[string]any{
		"model":      model,
		"max_tokens": 256,
		"system":     judge.System,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": judge.BuildUserContent(prompt, response, toolsSummary),
		}},
		"tools": []any{map[string]any{
			"name":        "record_outcome",
			"description": "Record the outcome verdict for this turn.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"verdict": map[string]any{
						"type": "string",
						"enum": []any{"success", "partial", "fail"},
					},
					"reason": map[string]any{"type": "string"},
				},
				"required": []any{"verdict"},
			},
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "record_outcome"},
	}
	disableThinking(reqBody, model)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", false
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	client := &http.Client{Timeout: judgeHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		hook.Logf("judge", "http: %s", err)
		return "", false
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		hook.Logf("judge", "http %d", resp.StatusCode)
		return "", false
	}

	var r struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		hook.Logf("judge", "decode: %s", err)
		return "", false
	}
	for _, c := range r.Content {
		if c.Type == "tool_use" && c.Name == "record_outcome" {
			var in struct {
				Verdict string `json:"verdict"`
			}
			if err := json.Unmarshal(c.Input, &in); err != nil {
				return "", false
			}
			if !judge.Valid(in.Verdict) {
				hook.Logf("judge", "invalid verdict %q", in.Verdict)
				return "", false
			}
			return in.Verdict, true
		}
	}
	hook.Logf("judge", "no record_outcome tool_use in response")
	return "", false
}

// cmdJudgeProbe judges a turn read as JSON from STDIN — a live-test
// harness for the judge wiring. Reads from stdin (not argv) so raw turn
// content never lands in `ps`/`/proc/<pid>/cmdline` or shell history.
// Prints the verdict to stderr. Requires HINDCAST_JUDGE=1 and a key.
//
// Usage: HINDCAST_JUDGE=1 hindcast _judge-probe   (then pipe
//
//	{"prompt":"...","response":"...","tools":"Edit×1"} on stdin)
func cmdJudgeProbe(_ []string) {
	if !judgeEnabled() {
		fmt.Fprintln(os.Stderr, "judge disabled — set HINDCAST_JUDGE=1")
		os.Exit(1)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || len(data) == 0 {
		fmt.Fprintln(os.Stderr, `usage: HINDCAST_JUDGE=1 hindcast _judge-probe  (reads {"prompt":..,"response":..,"tools":..} from stdin)`)
		os.Exit(2)
	}
	var in struct {
		Prompt   string `json:"prompt"`
		Response string `json:"response"`
		Tools    string `json:"tools"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		fmt.Fprintf(os.Stderr, "judge-probe: bad stdin JSON: %s\n", err)
		os.Exit(2)
	}
	if in.Prompt == "" {
		fmt.Fprintln(os.Stderr, "judge-probe: empty prompt")
		os.Exit(2)
	}
	v, model, ok := judgeTurn(in.Prompt, in.Response, in.Tools)
	if !ok {
		fmt.Fprintln(os.Stderr, "judge: no verdict (see hook.log judge entries — likely no API key or API error)")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "verdict=%s model=%s\n", v, model)
}

// toolsSummary renders a tool-call map as a stable "Edit×3, Bash×1"
// string (sorted by name) for the judge prompt. Empty when no tools ran.
func toolsSummary(calls map[string]int) string {
	if len(calls) == 0 {
		return ""
	}
	names := make([]string, 0, len(calls))
	for n := range calls {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", n, calls[n]))
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
