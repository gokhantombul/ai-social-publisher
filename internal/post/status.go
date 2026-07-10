package post

// Status is a post_job lifecycle state.
type Status string

const (
	StatusNew                    Status = "NEW"
	StatusScoringQueued          Status = "SCORING_QUEUED"
	StatusScoring                Status = "SCORING"
	StatusWaitingAI              Status = "WAITING_AI"
	StatusScored                 Status = "SCORED"
	StatusWaitingFirstApproval   Status = "WAITING_FIRST_APPROVAL"
	StatusVariantsQueued         Status = "VARIANTS_QUEUED"
	StatusGeneratingVariants     Status = "GENERATING_VARIANTS"
	StatusWaitingVariantApproval Status = "WAITING_VARIANT_APPROVAL"
	StatusReadyToPublish         Status = "READY_TO_PUBLISH"
	StatusScheduled              Status = "SCHEDULED"
	StatusApproved               Status = "APPROVED"
	StatusPublishing             Status = "PUBLISHING"
	StatusPublished              Status = "PUBLISHED"
	StatusSkipped                Status = "SKIPPED"
	StatusFailed                 Status = "FAILED"
)

// allowedTransitions encodes the controlled state machine. A job may only move
// from a status to one explicitly listed here.
var allowedTransitions = map[Status][]Status{
	StatusNew:                    {StatusScoringQueued, StatusFailed},
	StatusScoringQueued:          {StatusScoring, StatusFailed},
	StatusScoring:                {StatusWaitingAI, StatusScored, StatusFailed},
	StatusWaitingAI:              {StatusScoringQueued, StatusVariantsQueued, StatusFailed, StatusSkipped},
	StatusScored:                 {StatusWaitingFirstApproval, StatusSkipped, StatusFailed},
	StatusWaitingFirstApproval:   {StatusVariantsQueued, StatusSkipped, StatusFailed},
	StatusVariantsQueued:         {StatusGeneratingVariants, StatusSkipped, StatusFailed},
	StatusGeneratingVariants:     {StatusWaitingVariantApproval, StatusWaitingAI, StatusFailed},
	StatusWaitingVariantApproval: {StatusReadyToPublish, StatusVariantsQueued, StatusSkipped, StatusFailed},
	StatusReadyToPublish:         {StatusScheduled, StatusApproved, StatusVariantsQueued, StatusSkipped, StatusFailed},
	StatusScheduled:              {StatusApproved, StatusReadyToPublish, StatusSkipped, StatusFailed},
	StatusApproved:               {StatusPublishing, StatusFailed},
	StatusPublishing:             {StatusPublished, StatusFailed},
	// Terminal states.
	StatusPublished: {},
	StatusSkipped:   {},
	StatusFailed:    {},
}

// CanTransition reports whether moving from -> to is allowed. No-op transitions
// are deliberately rejected: accepting PUBLISHING -> PUBLISHING allowed two
// workers to perform the same external side effect.
func CanTransition(from, to Status) bool {
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
