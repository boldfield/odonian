package store

// nextTier returns the next tier in the model hierarchy.
// If the model is not found or is already the top tier, it returns empty string with ok=false.
func (s *sqliteStore) nextTier(model string) (string, bool) {
	for i, m := range s.escalationLadder {
		if m == model {
			// If this is the last element, we're at the top tier
			if i == len(s.escalationLadder)-1 {
				return "", false
			}
			// Return the next tier
			return s.escalationLadder[i+1], true
		}
	}
	// Model not found in the ladder
	return "", false
}

// isTopTier checks if the given model is the top tier.
func (s *sqliteStore) isTopTier(model string) bool {
	if len(s.escalationLadder) == 0 {
		return false
	}
	return s.escalationLadder[len(s.escalationLadder)-1] == model
}

// researchNextTier returns the next tier in the research escalation ladder.
// If the model is not found or is already the top tier, it returns empty string with ok=false.
// If the research ladder is empty, returns empty string with ok=false (no escalation).
func (s *sqliteStore) researchNextTier(model string) (string, bool) {
	// If research ladder is empty, no escalation is possible
	if len(s.researchEscalationLadder) == 0 {
		return "", false
	}

	for i, m := range s.researchEscalationLadder {
		if m == model {
			// If this is the last element, we're at the top tier
			if i == len(s.researchEscalationLadder)-1 {
				return "", false
			}
			// Return the next tier
			return s.researchEscalationLadder[i+1], true
		}
	}
	// Model not found in the ladder
	return "", false
}

// isResearchTopTier checks if the given model is the top tier in the research escalation ladder.
// Returns false if the research ladder is empty (no escalation configured).
func (s *sqliteStore) isResearchTopTier(model string) bool {
	if len(s.researchEscalationLadder) == 0 {
		return false
	}
	return s.researchEscalationLadder[len(s.researchEscalationLadder)-1] == model
}
