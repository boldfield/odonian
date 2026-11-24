package tuiview

const (
	MinWidth         = 40
	WideThreshold    = 120
	MediumThreshold  = 80
	ProjectPaneWidth = 17
	ProjectRailWidth = 10
	SeparatorWidth   = 1
)

type Layout struct {
	TooNarrow    bool
	ProjectWidth int // 0 if not shown
	ItemsWidth   int
	PreviewWidth int // 0 if not shown
}

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
		available -= SeparatorWidth
	}

	if itemSelected && !previewHidden {
		layout.PreviewWidth = available / 3
		layout.ItemsWidth = available - layout.PreviewWidth
		if layout.ProjectWidth > 0 {
			layout.ItemsWidth -= SeparatorWidth
		}
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
