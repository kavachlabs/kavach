package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// OpenCode's dark theme, with peach for Kavach and blue for the prompt.
var (
	colBg     = lipgloss.Color("#0a0a0a")
	colPanel  = lipgloss.Color("#1c1c1c")
	colHead   = lipgloss.Color("#151515")
	colSel    = lipgloss.Color("#303030")
	colText   = lipgloss.Color("#eeeeee")
	colDim    = lipgloss.Color("#808080")
	colFaint  = lipgloss.Color("#4a4a4a")
	colBlue   = lipgloss.Color("#5c9cf5")
	colPeach  = lipgloss.Color("#fab283")
	colGreen  = lipgloss.Color("#7fd88f")
	colRed    = lipgloss.Color("#e06c75")
	colOnPill = lipgloss.Color("#111111")
)

var (
	sText  = lipgloss.NewStyle().Foreground(colText)
	sBold  = lipgloss.NewStyle().Foreground(colText).Bold(true)
	sDim   = lipgloss.NewStyle().Foreground(colDim)
	sFaint = lipgloss.NewStyle().Foreground(colFaint)
	sBlue  = lipgloss.NewStyle().Foreground(colBlue)
	sPeach = lipgloss.NewStyle().Foreground(colPeach)
	sGreen = lipgloss.NewStyle().Foreground(colGreen)
	sRed   = lipgloss.NewStyle().Foreground(colRed)
)

// fill pads s to width on bg (none when bg is ""), cutting it if longer. Lip Gloss ends every
// styled run with a full reset, which would also drop the background, so
// the background (and the default text colour) is set again after each one.
func fill(s string, width int, bg lipgloss.Color) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) > width {
		s = ansi.Truncate(s, width-1, "") + "…"
	}
	pad := strings.Repeat(" ", width-lipgloss.Width(s))
	seq := bgCode(bg)
	if seq == "" {
		return s + pad
	}
	return seq + strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+seq) + pad + "\x1b[0m"
}

// bgCode is the escape sequence that sets bg and the default text colour;
// "" when colour is off.
func bgCode(bg lipgloss.Color) string {
	if bg == "" {
		return ""
	}
	r := lipgloss.NewStyle().Foreground(colText).Background(bg).Render("x")
	i := strings.IndexByte(r, 'x')
	if i < 0 {
		return ""
	}
	return r[:i]
}

// panel draws lines on a lighter background with a coloured edge on the
// left, as OpenCode draws its prompt and messages.
func panel(lines []string, width int, edge, bg lipgloss.Color) []string {
	out := make([]string, len(lines))
	e := lipgloss.NewStyle().Foreground(edge).Render("┃")
	for i, l := range lines {
		out[i] = e + fill("  "+l, width-1, bg)
	}
	return out
}

// spread puts left and right on one line of width.
func spread(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// center pads s on the left so it sits in the middle of width.
func center(s string, width int) string {
	if pad := (width - lipgloss.Width(s)) / 2; pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// hints renders key hints: the key bright, its label dim.
func hints(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, sText.Render(pairs[i])+" "+sDim.Render(pairs[i+1]))
	}
	return strings.Join(parts, "   ")
}

// table lays out rows in columns, each gap spaces wider than its widest
// cell. Cells may be styled.
func table(rows [][]string, gap int) []string {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		var b strings.Builder
		for j, c := range r {
			b.WriteString(c)
			if j < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[j]-lipgloss.Width(c)+gap))
			}
		}
		out[i] = b.String()
	}
	return out
}

func badge(status string) string {
	s := lipgloss.NewStyle().Foreground(colOnPill).Bold(true).Padding(0, 1)
	switch status {
	case "pass":
		return s.Background(colGreen).Render("PASS")
	case "fail":
		return s.Background(colRed).Render("FAIL")
	case "error":
		return s.Background(colDim).Render("ERROR")
	}
	return s.Background(colFaint).Render(strings.ToUpper(status))
}

// dot is an attempt's mark in the runs grid.
func dot(status string) string {
	switch status {
	case "pass":
		return sGreen.Render("●")
	case "fail":
		return sRed.Render("●")
	case "error":
		return sRed.Render("✕")
	case "running":
		return sPeach.Render("◐")
	}
	return sFaint.Render("○")
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", int64(math.Round(float64(n)/1e3)))
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func duration(sec float64) string {
	s := int(math.Round(sec))
	switch {
	case s >= 3600:
		return fmt.Sprintf("%dh %02dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}

func clock(sec float64) string {
	s := int(sec)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func dollars(c float64) string {
	if c > 0 && c < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", c)
}

func price(p *float64) string {
	switch {
	case p == nil:
		return "varies"
	case *p == 0:
		return "free"
	}
	return fmt.Sprintf("$%.2f", *p)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
