package outcome

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		prev string
		cur  string
		want Label
	}{
		{
			name: "near-identical re-ask is a redo",
			prev: "add a retry loop to the upload handler with backoff",
			cur:  "add the retry loop to the upload handler with backoff please",
			want: Redo,
		},
		{
			name: "explicit correction marker with topical overlap is a redo",
			prev: "rename the config field to MaxRetries",
			cur:  "no, rename the config field to RetryLimit",
			want: Redo,
		},
		{
			name: "marker alone without overlap is not enough",
			prev: "rename the config field to MaxRetries",
			cur:  "actually let's deploy the staging cluster now",
			want: MovedOn,
		},
		{
			name: "unrelated next task is moved_on",
			prev: "add a retry loop to the upload handler",
			cur:  "write release notes for v0.7",
			want: MovedOn,
		},
		{
			name: "empty prev cannot be a redo",
			prev: "",
			cur:  "no that's wrong, fix it",
			want: MovedOn,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.prev, c.cur, 0)
			if got.Label != c.want {
				t.Errorf("Classify(%q, %q) = %q (jaccard=%.2f marker=%v); want %q",
					c.prev, c.cur, got.Label, got.Jaccard, got.Marker, c.want)
			}
		})
	}
}

func TestClassifyRecordsFeatures(t *testing.T) {
	got := Classify("fix the parser bug", "no, fix the parser bug now", 42)
	if got.GapSec != 42 {
		t.Errorf("GapSec = %d; want 42", got.GapSec)
	}
	if !got.Marker {
		t.Errorf("expected marker=true for leading 'no,'")
	}
	if got.Jaccard <= 0 {
		t.Errorf("expected positive jaccard for overlapping prompts, got %.2f", got.Jaccard)
	}
}
