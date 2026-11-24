package main

import (
	keybind "charm.land/bubbles/v2/key"
)

// KeyBindings defines all key bindings for the TUI.
// Bindings are grouped by context (board, detail) and state.
type KeyBindings struct {
	// Board navigation
	Board boardKeyMap
	// Detail view
	Detail detailKeyMap
}

// boardKeyMap defines key bindings for the board view.
type boardKeyMap struct {
	// Navigation
	Left        keybind.Binding
	Right       keybind.Binding
	Up          keybind.Binding
	Down        keybind.Binding
	EnterDetail keybind.Binding

	// State-independent
	Hold          keybind.Binding
	SetPriority   keybind.Binding
	MoveToFront   keybind.Binding
	ResetPriority keybind.Binding
	Archive       keybind.Binding
	Scorecards    keybind.Binding
	SwitchProject keybind.Binding
	Refresh       keybind.Binding
	Quit          keybind.Binding

	// State-dependent (backlog only)
	Promote keybind.Binding

	// State-dependent (review only)
	Approve keybind.Binding
	Reject  keybind.Binding

	// State-dependent (approved only)
	Bounce keybind.Binding

	// State-dependent (blocked only)
	Unblock keybind.Binding
	Fail    keybind.Binding

	// Help
	Help keybind.Binding
}

// detailKeyMap defines key bindings for the detail view.
type detailKeyMap struct {
	Back          keybind.Binding
	ScrollUp      keybind.Binding
	ScrollDown    keybind.Binding
	PageUp        keybind.Binding
	PageDown      keybind.Binding
	OpenPR        keybind.Binding
	SourceDoc     keybind.Binding
	DesignDoc     keybind.Binding
	SwitchProject keybind.Binding
	SetPriority   keybind.Binding
	MoveToFront   keybind.Binding
	ResetPriority keybind.Binding

	// State-dependent (review only)
	Approve keybind.Binding
	Reject  keybind.Binding

	// State-dependent (approved only)
	MergeComplete keybind.Binding

	// Help
	Help keybind.Binding
}

// DefaultKeyBindings returns the default key bindings.
func DefaultKeyBindings() KeyBindings {
	return KeyBindings{
		Board: boardKeyMap{
			// Navigation
			Left: keybind.NewBinding(
				keybind.WithKeys("left"),
				keybind.WithHelp("←", "prev column"),
			),
			Right: keybind.NewBinding(
				keybind.WithKeys("right"),
				keybind.WithHelp("→", "next column"),
			),
			Up: keybind.NewBinding(
				keybind.WithKeys("up"),
				keybind.WithHelp("↑", "select up"),
			),
			Down: keybind.NewBinding(
				keybind.WithKeys("down"),
				keybind.WithHelp("↓", "select down"),
			),
			EnterDetail: keybind.NewBinding(
				keybind.WithKeys("enter"),
				keybind.WithHelp("enter", "detail"),
			),

			// State-independent
			Hold: keybind.NewBinding(
				keybind.WithKeys("t"),
				keybind.WithHelp("t", "hold/release"),
			),
			SetPriority: keybind.NewBinding(
				keybind.WithKeys("i"),
				keybind.WithHelp("i", "set priority"),
			),
			MoveToFront: keybind.NewBinding(
				keybind.WithKeys("w"),
				keybind.WithHelp("w", "move to front"),
			),
			ResetPriority: keybind.NewBinding(
				keybind.WithKeys("e"),
				keybind.WithHelp("e", "reset priority"),
			),
			Archive: keybind.NewBinding(
				keybind.WithKeys("z"),
				keybind.WithHelp("z", "archive"),
			),
			Scorecards: keybind.NewBinding(
				keybind.WithKeys("c"),
				keybind.WithHelp("c", "scorecards"),
			),
			SwitchProject: keybind.NewBinding(
				keybind.WithKeys("P"),
				keybind.WithHelp("P", "switch project"),
			),
			Refresh: keybind.NewBinding(
				keybind.WithKeys("r"),
				keybind.WithHelp("r", "refresh"),
			),
			Quit: keybind.NewBinding(
				keybind.WithKeys("q"),
				keybind.WithHelp("q", "quit"),
			),

			// State-dependent (backlog only)
			Promote: keybind.NewBinding(
				keybind.WithKeys("p"),
				keybind.WithHelp("p", "promote"),
			),

			// State-dependent (review only)
			Approve: keybind.NewBinding(
				keybind.WithKeys("a"),
				keybind.WithHelp("a", "approve"),
			),
			Reject: keybind.NewBinding(
				keybind.WithKeys("x"),
				keybind.WithHelp("x", "reject"),
			),

			// State-dependent (approved only)
			Bounce: keybind.NewBinding(
				keybind.WithKeys("b"),
				keybind.WithHelp("b", "bounce"),
			),

			// State-dependent (blocked only)
			Unblock: keybind.NewBinding(
				keybind.WithKeys("u"),
				keybind.WithHelp("u", "unblock"),
			),
			Fail: keybind.NewBinding(
				keybind.WithKeys("f"),
				keybind.WithHelp("f", "fail"),
			),

			// Help
			Help: keybind.NewBinding(
				keybind.WithKeys("?"),
				keybind.WithHelp("?", "help"),
			),
		},

		Detail: detailKeyMap{
			Back: keybind.NewBinding(
				keybind.WithKeys("esc"),
				keybind.WithHelp("esc", "back"),
			),
			ScrollUp: keybind.NewBinding(
				keybind.WithKeys("up"),
				keybind.WithHelp("↑", "scroll up"),
			),
			ScrollDown: keybind.NewBinding(
				keybind.WithKeys("down"),
				keybind.WithHelp("↓", "scroll down"),
			),
			PageUp: keybind.NewBinding(
				keybind.WithKeys("pgup"),
				keybind.WithHelp("pgup", "page up"),
			),
			PageDown: keybind.NewBinding(
				keybind.WithKeys("pgdn"),
				keybind.WithHelp("pgdn", "page down"),
			),
			OpenPR: keybind.NewBinding(
				keybind.WithKeys("o"),
				keybind.WithHelp("o", "open PR"),
			),
			SourceDoc: keybind.NewBinding(
				keybind.WithKeys("s"),
				keybind.WithHelp("s", "source doc"),
			),
			DesignDoc: keybind.NewBinding(
				keybind.WithKeys("d"),
				keybind.WithHelp("d", "design doc"),
			),
			SwitchProject: keybind.NewBinding(
				keybind.WithKeys("P"),
				keybind.WithHelp("P", "switch project"),
			),
			SetPriority: keybind.NewBinding(
				keybind.WithKeys("i"),
				keybind.WithHelp("i", "set priority"),
			),
			MoveToFront: keybind.NewBinding(
				keybind.WithKeys("w"),
				keybind.WithHelp("w", "move to front"),
			),
			ResetPriority: keybind.NewBinding(
				keybind.WithKeys("e"),
				keybind.WithHelp("e", "reset priority"),
			),

			// State-dependent (review only)
			Approve: keybind.NewBinding(
				keybind.WithKeys("a"),
				keybind.WithHelp("a", "approve"),
			),
			Reject: keybind.NewBinding(
				keybind.WithKeys("x"),
				keybind.WithHelp("x", "reject"),
			),

			// State-dependent (approved only)
			MergeComplete: keybind.NewBinding(
				keybind.WithKeys("m"),
				keybind.WithHelp("m", "merge & complete"),
			),

			// Help
			Help: keybind.NewBinding(
				keybind.WithKeys("?"),
				keybind.WithHelp("?", "help"),
			),
		},
	}
}

// getBoardBindingsForState returns the bindings that are valid for a given state.
// State-specific actions are listed first so they're visible at narrow widths.
func (kb *KeyBindings) getBoardBindingsForState(state string) []keybind.Binding {
	var bindings []keybind.Binding

	// State-specific bindings first (listed first so they don't get truncated at narrow widths)
	switch state {
	case stateBacklog:
		bindings = append(bindings, kb.Board.Promote)
	case stateReview:
		bindings = append(bindings,
			kb.Board.Approve,
			kb.Board.Reject,
		)
	case stateApproved:
		bindings = append(bindings, kb.Board.Bounce)
	case stateBlocked:
		bindings = append(bindings,
			kb.Board.Unblock,
			kb.Board.Fail,
		)
	}

	// Navigation bindings always available
	bindings = append(bindings,
		kb.Board.Left,
		kb.Board.Right,
		kb.Board.Up,
		kb.Board.Down,
		kb.Board.EnterDetail,
	)

	// State-independent bindings
	bindings = append(bindings,
		kb.Board.Hold,
		kb.Board.SetPriority,
		kb.Board.MoveToFront,
		kb.Board.ResetPriority,
		kb.Board.Archive,
		kb.Board.Scorecards,
		kb.Board.SwitchProject,
		kb.Board.Refresh,
		kb.Board.Quit,
		kb.Board.Help,
	)

	return bindings
}

// getDetailBindingsForState returns the bindings that are valid for the detail view in a given state.
// State-specific actions are listed first so they're visible at narrow widths.
func (kb *KeyBindings) getDetailBindingsForState(state string) []keybind.Binding {
	var bindings []keybind.Binding

	// State-specific bindings first (listed first so they don't get truncated at narrow widths)
	switch state {
	case stateReview:
		bindings = append(bindings,
			kb.Detail.Approve,
			kb.Detail.Reject,
		)
	case stateApproved:
		bindings = append(bindings, kb.Detail.MergeComplete)
	}

	// Navigation and scrolling
	bindings = append(bindings,
		kb.Detail.Back,
		kb.Detail.ScrollUp,
		kb.Detail.ScrollDown,
		kb.Detail.PageUp,
		kb.Detail.PageDown,
		kb.Detail.OpenPR,
		kb.Detail.SourceDoc,
		kb.Detail.DesignDoc,
		kb.Detail.SwitchProject,
		kb.Detail.SetPriority,
		kb.Detail.MoveToFront,
		kb.Detail.ResetPriority,
	)

	// Help is always available
	bindings = append(bindings, kb.Detail.Help)

	return bindings
}
