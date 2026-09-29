package remote

import (
	"strings"
	"testing"
)

// A host that never satisfies the challenge shape check can ask the same one
// legal single question again and again, drawing the cached password from the
// source on every one of them. The count is what stops it, so the count is
// what is tested: the first prompts are answered, the one past the cap is
// refused.
func TestKeyboardInteractiveChallengesAreCappedPerConnection(t *testing.T) {
	p := &passwordSource{}
	answered := 0
	for i := range maxKeyboardInteractiveChallenges {
		if err := p.claimChallenge(); err != nil {
			t.Fatalf("challenge %d refused below the cap: %v", i+1, err)
		}
		answered++
	}
	if answered != maxKeyboardInteractiveChallenges {
		t.Fatalf("answered %d challenges, want %d", answered, maxKeyboardInteractiveChallenges)
	}
	err := p.claimChallenge()
	if err == nil {
		t.Fatal("a challenge past the cap was accepted")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("refusal = %q, want it to name the cap", err)
	}
}

// The budget belongs to one connection: a later Connect gets a fresh
// passwordSource and the same prompts answered again.
func TestChallengeBudgetIsPerConnection(t *testing.T) {
	first := &passwordSource{}
	for range maxKeyboardInteractiveChallenges + 1 {
		_ = first.claimChallenge()
	}
	if err := (&passwordSource{}).claimChallenge(); err != nil {
		t.Errorf("a new connection must start with a fresh budget: %v", err)
	}
}
