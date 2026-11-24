package tuiview

// MinWidth is the minimum terminal width for the layout to be usable.
const MinWidth = 40

// WideThreshold is the minimum terminal width to show the full three-pane layout.
const WideThreshold = 120

// MediumThreshold is the minimum terminal width to show a project rail and items pane.
const MediumThreshold = 80

// ProjectPaneWidth is the width of the full project pane in wide layout.
const ProjectPaneWidth = 17

// ProjectRailWidth is the width of the project rail in medium layout.
const ProjectRailWidth = 10

// SeparatorWidth is the width of separators between panes.
const SeparatorWidth = 1

// Layout describes which panes are shown and their widths.
// Pane widths plus separators between shown panes must equal the terminal width.
type Layout struct {
	TooNarrow    bool // terminal is too narrow to render any useful layout
	ProjectWidth int  // 0 if not shown
	ItemsWidth   int  // always shown when not TooNarrow
	PreviewWidth int  // 0 if not shown
}

// ChooseLayout determines which panes to render at the given terminal size.
// width and height are terminal dimensions; itemSelected indicates whether a list item is selected;
// previewHidden and projectHidden are user toggle flags. height is reserved for future use.
func ChooseLayout(width, height int, itemSelected bool, previewHidden, projectHidden bool) Layout {
	if width < MinWidth {
		return Layout{TooNarrow: true}
	}

	if width >= WideThreshold {
		return layoutWide(width, itemSelected, previewHidden, projectHidden)
	} else if width >= MediumThreshold {
		return layoutMedium(width, projectHidden)
	} else {
		return layoutNarrow(width)
	}
}

func layoutWide(width int, itemSelected bool, previewHidden bool, projectHidden bool) Layout {
	layout := Layout{}

	if !projectHidden {
		layout.ProjectWidth = ProjectPaneWidth
	}

	available := width - layout.ProjectWidth
	if layout.ProjectWidth > 0 {
		available -= SeparatorWidth // project|items separator
	}

	if itemSelected && !previewHidden {
		available -= SeparatorWidth // items|preview separator (always needed when preview shown)
		layout.PreviewWidth = available / 3
		layout.ItemsWidth = available - layout.PreviewWidth
	} else {
		layout.ItemsWidth = available
	}

	return layout
}

func layoutMedium(width int, projectHidden bool) Layout {
	layout := Layout{}

	if !projectHidden {
		layout.ProjectWidth = ProjectRailWidth
		layout.ItemsWidth = width - layout.ProjectWidth - SeparatorWidth
	} else {
		layout.ItemsWidth = width
	}

	return layout
}

func layoutNarrow(width int) Layout {
	return Layout{ItemsWidth: width}
}
