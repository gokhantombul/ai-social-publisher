package post

// Status is a post_job lifecycle state.
type Status string

const (
	StatusNew                    Status = "NEW"
	StatusWaitingAI              Status = "WAITING_AI"
	StatusScored                 Status = "SCORED"
	StatusWaitingFirstApproval   Status = "WAITING_FIRST_APPROVAL"
	StatusGeneratingVariants     Status = "GENERATING_VARIANTS"
	StatusWaitingVariantApproval Status = "WAITING_VARIANT_APPROVAL"
	StatusApproved               Status = "APPROVED"
	StatusPublishing             Status = "PUBLISHING"
	StatusPublished              Status = "PUBLISHED"
	StatusSkipped                Status = "SKIPPED"
	StatusFailed                 Status = "FAILED"
)

// allowedTransitions encodes the controlled state machine. A job may only move
// from a status to one explicitly listed here.
var allowedTransitions = map[Status][]Status{
	StatusNew:                    {StatusWaitingAI, StatusScored, StatusFailed},
	StatusWaitingAI:              {StatusScored, StatusWaitingAI, StatusFailed, StatusSkipped},
	StatusScored:                 {StatusWaitingFirstApproval, StatusSkipped, StatusFailed},
	StatusWaitingFirstApproval:   {StatusGeneratingVariants, StatusSkipped, StatusFailed},
	StatusGeneratingVariants:     {StatusWaitingVariantApproval, StatusWaitingAI, StatusFailed},
	StatusWaitingVariantApproval: {StatusApproved, StatusGeneratingVariants, StatusSkipped, StatusFailed},
	StatusApproved:               {StatusPublishing, StatusFailed},
	StatusPublishing:             {StatusPublished, StatusFailed},
	// Terminal states.
	StatusPublished: {},
	StatusSkipped:   {},
	StatusFailed:    {},
}

// CanTransition reports whether moving from -> to is allowed. A no-op transition
// (from == to) is always allowed to keep retries idempotent.
func CanTransition(from, to Status) bool {
	if from == to {
		return true
	}
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether a status has no outgoing transitions.
func IsTerminal(s Status) bool {
	return s == StatusPublished || s == StatusSkipped || s == StatusFailed
}
