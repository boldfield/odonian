package main

import (
	"testing"
)

func TestThemeHasStatusStyleForEachState(t *testing.T) {
	theme := NewTheme()

	for _, state := range stateOrder {
		if _, ok := theme.StatusStyles[state]; !ok {
			t.Errorf("theme missing status style for state: %s", state)
		}
	}
}
