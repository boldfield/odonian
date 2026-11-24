package main

import (
	keybind "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// action names what a key does. Handlers switch on the action a binding resolves to, and the
// help bar is built from the same bindings, so the two cannot drift apart.
type action string

const (
	actQuit           action = "quit"
	actHelp           action = "help"
	actLeft           action = "left"
	actRight          action = "right"
	actUp             action = "up"
	actDown           action = "down"
	actEnter          action = "enter"
	actBack           action = "back"
	actPageUp         action = "pgup"
	actPageDown       action = "pgdown"
	actNextPane       action = "next-pane"
	actPrevPane       action = "prev-pane"
	actTogglePreview  action = "toggle-preview"
	actToggleProjects action = "toggle-projects"
	actOpenProject    action = "open-project"
	actArchiveProject action = "archive-project"
	actSwitchProject  action = "switch-project"
	actHold           action = "hold"
	actSetPriority    action = "set-priority"
	actMoveToFront    action = "move-to-front"
	actResetPriority  action = "reset-priority"
	actArchive        action = "archive"
	actScorecards     action = "scorecards"
	actRefresh        action = "refresh"
	actPromote        action = "promote"
	actApprove        action = "approve"
	actReject         action = "reject"
	actBounce         action = "bounce"
	actUnblock        action = "unblock"
	actFail           action = "fail"
	actMerge          action = "merge"
	actOpenPR         action = "open-pr"
	actSourceDoc      action = "source-doc"
	actDesignDoc      action = "design-doc"
)

// keyCtx is what decides which bindings are live: the focused shell pane and the state of
// the item the keys would act on. The tab layout and the detail view ignore the pane.
type keyCtx struct {
	pane  shellPane
	state string
}

// keyEntry binds an action to keys. A nil panes or states list means "any".
type keyEntry struct {
	act    action
	bind   keybind.Binding
	panes  []shellPane
	states []string
}

func (e keyEntry) valid(c keyCtx) bool {
	if len(e.panes) > 0 && !containsPane(e.panes, c.pane) {
		return false
	}
	if len(e.states) > 0 && !containsString(e.states, c.state) {
		return false
	}
	return true
}

func containsPane(ps []shellPane, p shellPane) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// keyMap is an ordered list of entries. The order is the help bar's order, so the primary
// actions of a state come first and survive truncation at narrow widths.
type keyMap []keyEntry

// lookup resolves a key press to the first live entry's action.
func (km keyMap) lookup(msg tea.KeyPressMsg, c keyCtx) (action, bool) {
	for _, e := range km {
		if e.valid(c) && keybind.Matches(msg, e.bind) {
			return e.act, true
		}
	}
	return "", false
}

// help lists the bindings live in the context, in order, skipping those with no help text.
func (km keyMap) help(c keyCtx) []keybind.Binding {
	var out []keybind.Binding
	for _, e := range km {
		if e.valid(c) && e.bind.Help().Key != "" {
			out = append(out, e.bind)
		}
	}
	return out
}

// binding returns the binding for an action (the zero Binding if there is none).
func (km keyMap) binding(a action) keybind.Binding {
	for _, e := range km {
		if e.act == a {
			return e.bind
		}
	}
	return keybind.Binding{}
}

// KeyBindings holds the bindings of each context.
type KeyBindings struct {
	Board  keyMap // the tab layout (legacy)
	Detail keyMap // the full-screen detail view
	Shell  keyMap // the three-pane shell
}

func bind(a action, keys, helpKey, helpDesc string, extra ...string) keyEntry {
	return keyEntry{act: a, bind: keybind.NewBinding(keybind.WithKeys(append([]string{keys}, extra...)...), keybind.WithHelp(helpKey, helpDesc))}
}

func (e keyEntry) inPanes(ps ...shellPane) keyEntry { e.panes = ps; return e }
func (e keyEntry) inStates(ss ...string) keyEntry   { e.states = ss; return e }

// hidden marks an entry as live but absent from the help (an alias row).
func (e keyEntry) hidden() keyEntry {
	e.bind.SetHelp("", "")
	return e
}

// DefaultKeyBindings returns the default key bindings.
func DefaultKeyBindings() KeyBindings {
	return KeyBindings{
		Board: keyMap{
			bind(actPromote, "p", "p", "promote").inStates(stateBacklog),
			bind(actApprove, "a", "a", "approve").inStates(stateReview),
			bind(actReject, "x", "x", "reject").inStates(stateReview),
			bind(actBounce, "b", "b", "bounce").inStates(stateApproved),
			bind(actUnblock, "u", "u", "unblock").inStates(stateBlocked),
			bind(actFail, "f", "f", "fail").inStates(stateBlocked),
			bind(actLeft, "left", "←/→", "column", "h"),
			bind(actRight, "right", "", "", "l").hidden(),
			bind(actUp, "up", "↑/↓", "select", "k"),
			bind(actDown, "down", "", "", "j").hidden(),
			bind(actEnter, "enter", "enter", "detail"),
			bind(actHold, "t", "t", "hold/release"),
			bind(actSetPriority, "i", "i", "set priority"),
			bind(actMoveToFront, "w", "w", "move to front"),
			bind(actResetPriority, "e", "e", "reset priority"),
			bind(actArchive, "z", "z", "archive"),
			bind(actScorecards, "c", "c", "scorecards"),
			bind(actRefresh, "r", "r", "refresh"),
			bind(actHelp, "?", "?", "help"),
			bind(actQuit, "q", "q", "quit", "ctrl+c"),
		},

		Detail: keyMap{
			bind(actApprove, "a", "a", "approve").inStates(stateReview),
			bind(actReject, "x", "x", "reject").inStates(stateReview),
			bind(actMerge, "m", "m", "merge & complete").inStates(stateApproved),
			bind(actBack, "esc", "esc", "back", "q"),
			bind(actHelp, "?", "?", "help"),
			bind(actSetPriority, "i", "i", "set priority"),
			bind(actMoveToFront, "w", "w", "move to front"),
			bind(actResetPriority, "e", "e", "reset priority"),
			bind(actUp, "up", "↑/↓", "scroll spec", "k"),
			bind(actDown, "down", "", "", "j").hidden(),
			bind(actPageUp, "pgup", "pgup/pgdn", "page"),
			bind(actPageDown, "pgdown", "", "").hidden(),
			bind(actOpenPR, "o", "o", "open PR"),
			bind(actSourceDoc, "s", "s", "source doc"),
			bind(actDesignDoc, "d", "d", "design doc"),
		},

		Shell: keyMap{
			// The selected item's state-specific actions come first.
			bind(actPromote, "p", "p", "promote").inPanes(paneItems).inStates(stateBacklog),
			bind(actApprove, "a", "a", "approve").inPanes(paneItems).inStates(stateReview),
			bind(actReject, "x", "x", "reject").inPanes(paneItems).inStates(stateReview),
			bind(actMerge, "m", "m", "merge").inPanes(paneItems).inStates(stateApproved),
			bind(actBounce, "b", "b", "bounce").inPanes(paneItems).inStates(stateApproved),
			bind(actUnblock, "u", "u", "unblock").inPanes(paneItems).inStates(stateBlocked),
			bind(actFail, "f", "f", "fail").inPanes(paneItems).inStates(stateBlocked),

			bind(actEnter, "enter", "enter", "detail").inPanes(paneItems, panePreview),
			bind(actOpenProject, "enter", "enter", "open").inPanes(paneProjects),
			bind(actNextPane, "tab", "tab", "pane"),
			bind(actPrevPane, "shift+tab", "", "").hidden(),
			bind(actUp, "up", "↑/↓", "move", "k"),
			bind(actDown, "down", "", "", "j").hidden(),
			bind(actHold, "t", "t", "hold/release").inPanes(paneItems),
			bind(actSetPriority, "i", "i", "priority").inPanes(paneItems),
			bind(actMoveToFront, "w", "w", "front").inPanes(paneItems),
			bind(actResetPriority, "e", "e", "reset").inPanes(paneItems),
			bind(actArchive, "z", "z", "archive").inPanes(paneItems),
			bind(actArchiveProject, "z", "z", "archive project").inPanes(paneProjects),
			bind(actTogglePreview, "v", "v", "preview"),
			bind(actToggleProjects, "V", "V", "projects"),
			bind(actScorecards, "c", "c", "scorecards"),
			bind(actRefresh, "r", "r", "refresh"),
			bind(actHelp, "?", "?", "help"),
			bind(actQuit, "q", "q", "quit", "ctrl+c"),
		},
	}
}
