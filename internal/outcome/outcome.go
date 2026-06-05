// Package outcome derives a coarse, deterministic success/failure label
// for a completed turn from *the next turn's behavior* — the cheap,
// objective half of the dual-label scheme (see
// FEATURE_REQUEST_outcome-calibration.md). No model calls, no network.
//
// The signal: if turn N+1 looks like the user redoing / correcting /
// reverting turn N, that's evidence turn N did not land. Otherwise the
// user moved on, weak evidence it did. This is a proxy — it's blind to
// silent failures the user didn't catch and to successes the user
// happened not to follow up on — but it's grounded in revealed behavior,
// which is exactly what makes it the validation anchor for the (biased)
// LLM judge added in increment 2.
package outcome

import "strings"

// Label is the behavioral outcome of a turn, inferred one turn later.
type Label string

const (
	// Redo: the following turn re-asks, corrects, or reverts this turn.
	Redo Label = "redo"
	// MovedOn: the following turn went somewhere else — weak success signal.
	MovedOn Label = "moved_on"
	// Unknown: not enough signal to classify (e.g. no following turn).
	Unknown Label = ""
)

// Result carries the label plus the features behind it, so the report
// can show *why* a turn was labeled and the thresholds can be tuned
// against real data before anything depends on them.
type Result struct {
	Label   Label   `json:"label"`
	Jaccard float64 `json:"jaccard"`           // token-set overlap of the two prompts
	Marker  bool    `json:"marker"`            // next prompt opens with a correction cue
	GapSec  int     `json:"gap_sec,omitempty"` // seconds from this turn's stop to the next prompt
}

// Tunable thresholds. Deliberately conservative for v1 — we'd rather
// under-call "redo" (and validate/loosen later against the judge) than
// flood the log with false failures. All exported so tests and a future
// tune step can probe them.
const (
	// JaccardRedo: prompt overlap at/above this alone implies a redo —
	// the user re-asking substantially the same thing.
	JaccardRedo = 0.5
	// JaccardMarkerFloor: with an explicit correction cue, a lower
	// overlap floor still counts (the cue carries the signal, the
	// overlap just confirms it's about the same work).
	JaccardMarkerFloor = 0.2
)

// correctionMarkers are leading cues that the next prompt is pushing back
// on the previous turn rather than starting fresh work. Matched against
// the lowercased, trimmed start of the prompt.
var correctionMarkers = []string{
	"no,", "no ", "nope", "actually", "wait,", "wait ",
	"revert", "undo", "that's wrong", "thats wrong", "that's not",
	"thats not", "not what", "doesn't work", "doesnt work",
	"that broke", "you broke", "still broken", "still failing",
	"still doesn't", "still doesnt", "try again", "instead",
}

// Classify infers turn N's label from N's prompt, N+1's prompt, and the
// gap between them. prevPrompt is turn N (the turn being labeled),
// curPrompt is turn N+1 (the evidence). gapSec is seconds from N's stop
// to N+1's prompt; pass 0 if unknown (gap is recorded as a feature, not
// a gate, so it never changes the label on its own).
func Classify(prevPrompt, curPrompt string, gapSec int) Result {
	r := Result{Label: MovedOn, GapSec: gapSec}
	prevTokens := tokenSet(prevPrompt)
	curTokens := tokenSet(curPrompt)
	if len(prevTokens) == 0 || len(curTokens) == 0 {
		// One side is empty — no basis to call a redo. Default MovedOn
		// is still weak signal; an empty prompt is rare in practice.
		return r
	}
	r.Jaccard = jaccard(prevTokens, curTokens)
	r.Marker = hasCorrectionMarker(curPrompt)

	switch {
	case r.Jaccard >= JaccardRedo:
		r.Label = Redo
	case r.Marker && r.Jaccard >= JaccardMarkerFloor:
		r.Label = Redo
	}
	return r
}

func tokenSet(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, f := range strings.Fields(strings.ToLower(s)) {
		f = strings.Trim(f, ".,!?;:\"'()[]{}`")
		if len(f) < 2 {
			continue // drop single chars / stray punctuation
		}
		set[f] = struct{}{}
	}
	return set
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func hasCorrectionMarker(s string) bool {
	head := strings.ToLower(strings.TrimSpace(s))
	for _, m := range correctionMarkers {
		if strings.HasPrefix(head, m) {
			return true
		}
	}
	return false
}
