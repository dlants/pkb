package embed

import "testing"

func TestSplitBatchesCapsCohereLimit(t *testing.T) {
	// Bedrock Cohere embed-v4 rejects >96 texts per request; splitBatches must
	// keep every batch within the cap while preserving order and coverage.
	mk := func(n int) []string {
		s := make([]string, n)
		for i := range s {
			s[i] = string(rune('a' + i%26))
		}
		return s
	}
	cases := []struct {
		n, want int // n texts -> want batch count at size 96
	}{
		{0, 0}, {1, 1}, {96, 1}, {97, 2}, {192, 2}, {193, 3}, {1114, 12},
	}
	for _, c := range cases {
		texts := mk(c.n)
		batches := splitBatches(texts, maxCohereBatchTexts)
		if len(batches) != c.want {
			t.Errorf("n=%d: got %d batches, want %d", c.n, len(batches), c.want)
		}
		total := 0
		var flat []string
		for _, b := range batches {
			if len(b) > maxCohereBatchTexts {
				t.Errorf("n=%d: batch of %d exceeds cap %d", c.n, len(b), maxCohereBatchTexts)
			}
			total += len(b)
			flat = append(flat, b...)
		}
		if total != c.n {
			t.Errorf("n=%d: batches cover %d texts, want %d", c.n, total, c.n)
		}
		for i := range texts {
			if flat[i] != texts[i] {
				t.Errorf("n=%d: order not preserved at %d", c.n, i)
				break
			}
		}
	}
}
