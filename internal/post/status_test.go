package post

import "testing"

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to Status
		want     bool
	}{
		{StatusNew, StatusScoringQueued, true},
		{StatusScoringQueued, StatusScoring, true},
		{StatusScoring, StatusScored, true},
		{StatusScoring, StatusWaitingAI, true},
		{StatusScored, StatusWaitingFirstApproval, true},
		{StatusWaitingFirstApproval, StatusVariantsQueued, true},
		{StatusVariantsQueued, StatusGeneratingVariants, true},
		{StatusGeneratingVariants, StatusWaitingVariantApproval, true},
		{StatusWaitingVariantApproval, StatusReadyToPublish, true},
		{StatusReadyToPublish, StatusApproved, true},
		{StatusWaitingVariantApproval, StatusApproved, false},
		{StatusApproved, StatusPublishing, true},
		{StatusPublishing, StatusPublished, true},
		// Disallowed jumps.
		{StatusNew, StatusPublished, false},
		{StatusScored, StatusApproved, false},
		{StatusPublished, StatusPublishing, false},
		{StatusSkipped, StatusApproved, false},
		// No-op transitions are rejected so they cannot repeat side effects.
		{StatusWaitingAI, StatusWaitingAI, false},
		{StatusPublishing, StatusPublishing, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%s,%s)=%v want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []Status{StatusPublished, StatusSkipped, StatusFailed} {
		if !IsTerminal(s) {
			t.Errorf("expected %s to be terminal", s)
		}
	}
	if IsTerminal(StatusScored) {
		t.Error("SCORED should not be terminal")
	}
}
