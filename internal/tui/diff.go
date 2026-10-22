package tui

import "strings"

// colorizeDiff applies simple unified-diff colouring.
func colorizeDiff(patch string) string {
	lines := strings.Split(patch, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "diff "):
			lines[i] = styleBold.Render(l)
		case strings.HasPrefix(l, "@@"):
			lines[i] = styleTitle.Render(l)
		case strings.HasPrefix(l, "+"):
			lines[i] = statusStyle("working").Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = styleErr.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
