package api

import "testing"

func TestRatioDamping(t *testing.T) {
	if r := ratio(1, 0); r >= suggestScore {
		t.Errorf("1 applied/0 dismissed should be well below suggestScore, got %v", r)
	}
	if r := ratio(20, 0); r < autoApplyScore {
		t.Errorf("20 applied/0 dismissed should clear autoApplyScore, got %v", r)
	}
	if r := ratio(0, 5); r != 0 {
		t.Errorf("0 applied should always score 0 regardless of dismissals, got %v", r)
	}
}

func TestTokenizeSubject(t *testing.T) {
	got := tokenizeSubject("Re: Your Invoice #4821 is Ready!")
	want := map[string]bool{"your": false, "invoice": true, "ready": true}
	gotSet := map[string]bool{}
	for _, tok := range got {
		gotSet[tok] = true
	}
	for tok, shouldHave := range want {
		if gotSet[tok] != shouldHave {
			t.Errorf("token %q: got present=%v, want %v (tokens: %v)", tok, gotSet[tok], shouldHave, got)
		}
	}
}

func TestJaccard(t *testing.T) {
	if j := jaccard(nil, []string{"a"}); j != 0 {
		t.Errorf("empty set should score 0, got %v", j)
	}
	a := []string{"invoice", "payment", "due"}
	b := []string{"invoice", "payment", "overdue"}
	if j := jaccard(a, b); j <= 0.3 || j >= 1 {
		t.Errorf("partial overlap should land strictly between 0.3 and 1, got %v", j)
	}
	if j := jaccard(a, a); j != 1 {
		t.Errorf("identical sets should score 1, got %v", j)
	}
}
