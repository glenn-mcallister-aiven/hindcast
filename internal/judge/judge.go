// Package judge holds the model-independent pieces of the Stop-time
// outcome judge — the dense, biased half of the dual-label scheme. The
// network call lives in package main (cmd_judge.go); everything here is
// pure so it's testable without an API key: the cache key, the rubric,
// and verdict parsing.
//
// The judge grades a single completed turn from its own content. It is a
// post-hoc rater and therefore prone to hindsight bias (it tends to see
// success in whatever happened) — which is exactly why its verdicts are
// validated against the objective behavioral signal before anything
// depends on them. See FEATURE_REQUEST_outcome-calibration.md.
package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Verdict is the judge's coarse grade for a turn.
type Verdict string

const (
	Success Verdict = "success" // did what was asked, looks complete
	Partial Verdict = "partial" // progress but unfinished or partly wrong
	Fail    Verdict = "fail"    // did not accomplish the ask / went wrong
)

// RubricVersion is bumped whenever the rubric or input framing changes,
// so it can join the cache key — a reworded rubric must not return a
// stale cached verdict from the old one.
const RubricVersion = "v1"

// Valid reports whether s is a verdict the judge is allowed to emit.
func Valid(s string) bool {
	switch Verdict(s) {
	case Success, Partial, Fail:
		return true
	}
	return false
}

// System is the grader's instruction. Deterministic, terse, and biased
// toward "partial" on uncertainty so the judge doesn't inflate.
const System = `You are grading whether a single AI coding-assistant turn accomplished what the user asked, judging ONLY from the turn's own content shown to you. Grade the work, not its tone or confidence.

Call record_outcome exactly once with one verdict:
- success: the turn did what was asked and looks complete.
- partial: it made real progress but left the task unfinished or partly wrong.
- fail: it did not accomplish the ask, or went wrong.

When genuinely unsure, prefer "partial". Do not reward a confident-sounding summary that isn't backed by the actual work shown.`

// BuildUserContent renders the turn into the grader's user message.
// toolsSummary is a compact "Edit×3, Bash×1" style string (may be empty).
func BuildUserContent(prompt, response, toolsSummary string) string {
	var b strings.Builder
	b.WriteString("USER ASKED:\n")
	b.WriteString(strings.TrimSpace(prompt))
	b.WriteString("\n\nASSISTANT RESPONSE (text):\n")
	resp := strings.TrimSpace(response)
	if resp == "" {
		resp = "(no assistant text — tool calls only)"
	}
	b.WriteString(resp)
	if toolsSummary != "" {
		b.WriteString("\n\nTOOLS USED: ")
		b.WriteString(toolsSummary)
	}
	return b.String()
}

// CacheKey hashes every input that can change the verdict — model,
// rubric version, the rubric TEXT itself, and the full turn content — so
// a hit is safe to reuse and any change misses. Folding in the System
// string means an edit to the rubric auto-invalidates stale verdicts
// even if someone forgets to bump RubricVersion. Returns a hex string
// usable as a filename.
func CacheKey(model, prompt, response, toolsSummary string) string {
	h := sha256.New()
	// Length-prefix each field so concatenation can't collide across
	// boundaries (e.g. prompt "ab"+response "c" vs "a"+"bc").
	for _, s := range []string{RubricVersion, System, model, prompt, response, toolsSummary} {
		fmt.Fprintf(h, "%d:", len(s))
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// shapePatterns match unambiguous secret SHAPES — fixed prefixes/formats
// with negligible false-positive risk — so they can be blanket-replaced.
var shapePatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN[^-]*-----[\s\S]*?-----END[^-]*-----`),                // PEM blocks
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                                         // OpenAI/Anthropic-style keys
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                              // AWS access key id
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                                    // GitHub tokens
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                                  // Slack tokens
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),                                         // Google API keys
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // JWTs
}

// basicAuthURL matches credentials embedded in a URL (https://user:pass@host,
// postgres://user:pass@host); the scheme+host are kept, the creds dropped.
var basicAuthURL = regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)

// kvSecret matches `key = value` style assignments. The separator is
// MANDATORY (an optional one over-matched prose like "secret handling"),
// and the value is only redacted by the func below if it actually looks
// secret-like — otherwise "token: implementation" would be scrubbed.
var kvSecret = regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password|passwd|authorization|bearer)\b\s*[:=]\s*['"]?([A-Za-z0-9._\-/+]{8,})`)

// Redact scrubs common secret shapes from text before it is sent to the
// judge API. Best-effort defense-in-depth, NOT a guarantee — it catches
// the high-frequency shapes (provider keys, AWS keys, PEM blocks, JWTs,
// basic-auth URLs, secret-looking key=value pairs) and replaces them with
// a placeholder. The judge only needs to know whether the work landed,
// not the secret bytes. Deliberately conservative on key=value to avoid
// mangling ordinary code/prose, which would degrade the judge's input.
func Redact(s string) string {
	for _, re := range shapePatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	s = basicAuthURL.ReplaceAllString(s, "${1}[REDACTED]@")
	s = kvSecret.ReplaceAllStringFunc(s, func(m string) string {
		sub := kvSecret.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		val := sub[1]
		// Only redact values that look like secrets, not dictionary words:
		// a secret almost always carries a digit, or is improbably long.
		// "implementation"/"middleware" (no digit, <24) are left intact.
		if !strings.ContainsAny(val, "0123456789") && len(val) < 24 {
			return m
		}
		// Keep the key+separator for the judge's context; drop the value.
		return strings.TrimSuffix(m, val) + "[REDACTED]"
	})
	return s
}
