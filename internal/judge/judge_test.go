package judge

import "testing"

func TestValid(t *testing.T) {
	for _, v := range []string{"success", "partial", "fail"} {
		if !Valid(v) {
			t.Errorf("Valid(%q) = false; want true", v)
		}
	}
	for _, v := range []string{"", "ok", "SUCCESS", "good"} {
		if Valid(v) {
			t.Errorf("Valid(%q) = true; want false", v)
		}
	}
}

func TestCacheKeyDeterministic(t *testing.T) {
	a := CacheKey("claude-haiku-4-5", "do X", "did X", "Edit×1")
	b := CacheKey("claude-haiku-4-5", "do X", "did X", "Edit×1")
	if a != b {
		t.Errorf("CacheKey not deterministic: %s != %s", a, b)
	}
}

func TestCacheKeySensitivity(t *testing.T) {
	base := CacheKey("claude-haiku-4-5", "do X", "did X", "Edit×1")
	cases := map[string]string{
		"model":    CacheKey("claude-opus-4-8", "do X", "did X", "Edit×1"),
		"prompt":   CacheKey("claude-haiku-4-5", "do Y", "did X", "Edit×1"),
		"response": CacheKey("claude-haiku-4-5", "do X", "did Y", "Edit×1"),
		"tools":    CacheKey("claude-haiku-4-5", "do X", "did X", "Bash×1"),
	}
	for field, k := range cases {
		if k == base {
			t.Errorf("CacheKey collided when %s changed", field)
		}
	}
}

// Field-boundary collision: distinct splits of the same concatenated
// bytes must not produce the same key (length-prefixing guards this).
func TestCacheKeyNoBoundaryCollision(t *testing.T) {
	a := CacheKey("m", "ab", "c", "")
	b := CacheKey("m", "a", "bc", "")
	if a == b {
		t.Errorf("CacheKey collided across a field boundary")
	}
}

func TestBuildUserContentHandlesEmptyResponse(t *testing.T) {
	got := BuildUserContent("do the thing", "", "Bash×2")
	if got == "" {
		t.Fatal("expected non-empty content")
	}
	// Must note the tool-only case rather than render a blank response.
	if want := "tool calls only"; !contains(got, want) {
		t.Errorf("expected %q in content, got:\n%s", want, got)
	}
	if !contains(got, "Bash×2") {
		t.Errorf("expected tools summary in content")
	}
}

func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		gone string // substring that must NOT survive
	}{
		{"anthropic key", "here is sk-ant-api03-AbCdEf0123456789xyz the key", "sk-ant-api03-AbCdEf0123456789xyz"},
		{"aws key id", "AKIAIOSFODNN7EXAMPLE leaked", "AKIAIOSFODNN7EXAMPLE"},
		{"github token", "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"},
		{"key=value with digit", "API_KEY=supersecretvalue123", "supersecretvalue123"},
		{"token= with digit", "export TOKEN=abc123def456ghi789", "abc123def456ghi789"},
		{"pem block", "-----BEGIN PRIVATE KEY-----\nMIIBVgIBADAN\n-----END PRIVATE KEY-----", "MIIBVgIBADAN"},
		{"google key", "key AIzaSyA1234567890abcdefGHIJKLMNOPqrstuvw works", "AIzaSyA1234567890abcdefGHIJKLMNOPqrstuvw"},
		{"basic-auth url", "connect to postgres://admin:hunter2pw@db.internal/app", "admin:hunter2pw"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if contains(got, c.gone) {
				t.Errorf("Redact left secret in output: %q still contains %q", got, c.gone)
			}
			if !contains(got, "[REDACTED]") {
				t.Errorf("expected [REDACTED] marker, got %q", got)
			}
		})
	}
}

func TestRedactLeavesNormalCode(t *testing.T) {
	code := "func add(a, b int) int { return a + b } // computes the sum"
	if got := Redact(code); got != code {
		t.Errorf("Redact mangled normal code:\n in:  %q\n out: %q", code, got)
	}
}

// The conservative key=value rule must NOT scrub ordinary prose that
// merely contains a trigger word — that would degrade the judge's input.
func TestRedactLeavesBenignTriggerProse(t *testing.T) {
	benign := []string{
		"the token: implementation needs review",
		"secret handling logic needs review",
		"refactor the authorization middleware chain",
		"document the api_key configuration section",
		"bearer of bad news about the parser",
		"the password reset flow is broken",
	}
	for _, s := range benign {
		if got := Redact(s); got != s {
			t.Errorf("Redact over-scrubbed benign prose:\n in:  %q\n out: %q", s, got)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
