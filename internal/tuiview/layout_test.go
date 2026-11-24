package tuiview

import (
	"testing"
)

func TestChooseLayout(t *testing.T) {
	tests := []struct {
		name           string
		width          int
		height         int
		itemSelected   bool
		previewHidden  bool
		projectHidden  bool
		wantTooNarrow  bool
		checkWidthsSum bool
	}{
		// Minimum width boundary
		{name: "below minimum 39", width: 39, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: true, checkWidthsSum: false},
		{name: "at minimum 40", width: 40, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},

		// Narrow layout (< 80)
		{name: "narrow 60 no selection", width: 60, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 60 selected", width: 60, height: 10, itemSelected: true, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 60 preview hidden", width: 60, height: 10, itemSelected: true, previewHidden: true, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 60 project hidden", width: 60, height: 10, itemSelected: false, previewHidden: false, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 60 both hidden", width: 60, height: 10, itemSelected: false, previewHidden: true, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},

		{name: "narrow 79 no selection", width: 79, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 79 selected", width: 79, height: 10, itemSelected: true, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 79 preview hidden", width: 79, height: 10, itemSelected: true, previewHidden: true, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 79 project hidden", width: 79, height: 10, itemSelected: false, previewHidden: false, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},
		{name: "narrow 79 both hidden", width: 79, height: 10, itemSelected: false, previewHidden: true, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},

		// Medium layout (80-119)
		{name: "medium 80 no selection", width: 80, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 80 selected", width: 80, height: 10, itemSelected: true, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 80 preview hidden", width: 80, height: 10, itemSelected: true, previewHidden: true, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 80 project hidden", width: 80, height: 10, itemSelected: false, previewHidden: false, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 80 both hidden", width: 80, height: 10, itemSelected: false, previewHidden: true, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},

		{name: "medium 119 no selection", width: 119, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 119 selected", width: 119, height: 10, itemSelected: true, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 119 preview hidden", width: 119, height: 10, itemSelected: true, previewHidden: true, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 119 project hidden", width: 119, height: 10, itemSelected: false, previewHidden: false, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},
		{name: "medium 119 both hidden", width: 119, height: 10, itemSelected: false, previewHidden: true, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},

		// Wide layout (120+)
		{name: "wide 120 no selection", width: 120, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 120 selected", width: 120, height: 10, itemSelected: true, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 120 preview hidden", width: 120, height: 10, itemSelected: true, previewHidden: true, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 120 project hidden", width: 120, height: 10, itemSelected: false, previewHidden: false, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 120 both hidden", width: 120, height: 10, itemSelected: false, previewHidden: true, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},

		{name: "wide 200 no selection", width: 200, height: 10, itemSelected: false, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 200 selected", width: 200, height: 10, itemSelected: true, previewHidden: false, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 200 preview hidden", width: 200, height: 10, itemSelected: true, previewHidden: true, projectHidden: false, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 200 project hidden", width: 200, height: 10, itemSelected: false, previewHidden: false, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},
		{name: "wide 200 both hidden", width: 200, height: 10, itemSelected: false, previewHidden: true, projectHidden: true, wantTooNarrow: false, checkWidthsSum: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := ChooseLayout(tt.width, tt.height, tt.itemSelected, tt.previewHidden, tt.projectHidden)

			if layout.TooNarrow != tt.wantTooNarrow {
				t.Errorf("TooNarrow = %v, want %v", layout.TooNarrow, tt.wantTooNarrow)
			}

			if tt.checkWidthsSum {
				sum := layout.ProjectWidth + layout.ItemsWidth + layout.PreviewWidth

				// Count separators: one between project and items if both shown, one between items and preview if both shown
				separators := 0
				if layout.ProjectWidth > 0 && layout.ItemsWidth > 0 {
					separators++
				}
				if layout.ItemsWidth > 0 && layout.PreviewWidth > 0 {
					separators++
				}

				sum += separators * SeparatorWidth

				if sum != tt.width {
					t.Errorf("widths sum = %d (project=%d + items=%d + preview=%d + separators=%d), want %d",
						sum, layout.ProjectWidth, layout.ItemsWidth, layout.PreviewWidth, separators*SeparatorWidth, tt.width)
				}
			}

			// Verify layout rules
			if !layout.TooNarrow {
				// Items should always be shown (ItemsWidth > 0)
				if layout.ItemsWidth <= 0 {
					t.Errorf("ItemsWidth = %d, want > 0", layout.ItemsWidth)
				}

				// Verify threshold rules
				if tt.width >= WideThreshold {
					// At wide threshold, project pane should be shown unless hidden
					if !tt.projectHidden && layout.ProjectWidth != ProjectPaneWidth {
						t.Errorf("at wide threshold: ProjectWidth = %d, want %d", layout.ProjectWidth, ProjectPaneWidth)
					}

					// Preview should only show if item selected and preview not hidden
					if tt.itemSelected && !tt.previewHidden {
						if layout.PreviewWidth <= 0 {
							t.Errorf("at wide threshold with item selected and preview enabled: PreviewWidth = %d, want > 0", layout.PreviewWidth)
						}
					} else {
						if layout.PreviewWidth != 0 {
							t.Errorf("at wide threshold without item or with preview hidden: PreviewWidth = %d, want 0", layout.PreviewWidth)
						}
					}
				} else if tt.width >= MediumThreshold {
					// At medium threshold, should not show preview
					if layout.PreviewWidth != 0 {
						t.Errorf("at medium threshold: PreviewWidth = %d, want 0", layout.PreviewWidth)
					}

					// Project rail should be shown unless hidden
					if !tt.projectHidden && layout.ProjectWidth != ProjectRailWidth {
						t.Errorf("at medium threshold: ProjectWidth = %d, want %d", layout.ProjectWidth, ProjectRailWidth)
					}
				} else {
					// At narrow threshold, neither projects nor preview should show
					if layout.ProjectWidth != 0 {
						t.Errorf("at narrow threshold: ProjectWidth = %d, want 0", layout.ProjectWidth)
					}
					if layout.PreviewWidth != 0 {
						t.Errorf("at narrow threshold: PreviewWidth = %d, want 0", layout.PreviewWidth)
					}
				}
			}
		})
	}
}
