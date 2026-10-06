package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kavachlabs/kavach"
)

// 256-color palette carried over from the original kavach CLI.
const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiRed     = "\033[38;5;196m"
	ansiGreen   = "\033[38;5;46m"
	ansiYellow  = "\033[38;5;220m"
	ansiBlue    = "\033[38;5;39m"
	ansiOrange  = "\033[38;5;208m"
	ansiCyan    = "\033[38;5;51m"
	ansiMagenta = "\033[38;5;201m"
	ansiOnBlack = "\033[38;5;16m"
)

// ui renders human output. With color off it adds nothing, so piped output,
// --json and NO_COLOR stay plain and stable for scripts and agents.
type ui struct {
	color  bool
	banner bool
}

// newUI decides styling for w: color only on a terminal, never with a
// non-empty NO_COLOR or TERM=dumb; FORCE_COLOR turns it on regardless. The banner follows color
// unless disabled.
func newUI(w io.Writer, noBanner bool) ui {
	color := isTerminal(w) && os.Getenv("TERM") != "dumb"
	if os.Getenv("FORCE_COLOR") != "" {
		color = true
	}
	if os.Getenv("NO_COLOR") != "" { // https://no-color.org
		color = false
	}
	return ui{color: color, banner: color && !noBanner && os.Getenv("KAVACH_NO_BANNER") == ""}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// paint wraps s in the given styles when color is on.
func (u ui) paint(s string, styles ...string) string {
	if !u.color || s == "" {
		return s
	}
	return strings.Join(styles, "") + s + ansiReset
}

// label renders the left-hand column of a key/value line.
func (u ui) label(s string, width int) string {
	return u.paint(fmt.Sprintf("%-*s", width, s), ansiBold)
}

var bannerMark = []string{
	"  ⣠⣴⠶⠾⠷⠶⣦⡄  ",
	"⢀⣾⠋⣠⡶⠶⠶⠾⠋ ⣶⡀",
	"⣸⡇⢸⡏    ⢹⡇⠸⣇",
	"⢹⡇⢸⣇    ⣸⡇⢰⡏",
	"⠈⢿⣄⠙⠷⠶⠶⠾⠋⣠⡿⠁",
	"  ⠙⠻⠶⢦⡴⠶⠟⠋  ",
}

var bannerText = []string{
	"██                                           ██      ",
	"██  ▄█▀   ▄████▄  ██    ██  ▄████▄   ▄████▄  ██      ",
	"█████▄        ██  ██    ██      ██  ██    ▀  ██████▄ ",
	"██  ▀██   ▄█████   ▀█  █▀   ▄█████  ██       ██   ██ ",
	"██   ███ ██   ██    ▀██▀   ██   ██  ██    ▄  ██   ██ ",
	"██    ██  ▀█████     ▀▀     ▀█████   ▀████▀  ██   ██ ",
}

// printBanner prints the kada mark and wordmark, if the banner is enabled.
func (u ui) printBanner(w io.Writer) {
	if !u.banner {
		return
	}
	fmt.Fprintln(w)
	for i := range bannerMark {
		fmt.Fprintf(w, "  %s   %s\n", u.paint(bannerMark[i], ansiBold, ansiCyan), u.paint(bannerText[i], ansiBold, ansiOrange))
	}
	fmt.Fprintf(w, "  %s\n\n", u.paint("deterministic replay and fix verification for Go services · "+kavach.Version, ansiDim))
}

// header prints a section title such as "=== [KAVACH REPLAY] ===".
func (u ui) header(w io.Writer, title string) {
	if !u.color {
		return
	}
	fmt.Fprintln(w, u.paint("=== ["+title+"] ===", ansiBold, ansiCyan))
}

var statusStyle = map[kavach.Status]struct{ bg, fg, text string }{
	kavach.StatusFixed:             {"\033[48;5;46m", ansiGreen, "FIXED"},
	kavach.StatusOK:                {"\033[48;5;46m", ansiGreen, "OK"},
	kavach.StatusStillFailing:      {"\033[48;5;196m", ansiRed, "STILL FAILING"},
	kavach.StatusDiverged:          {"\033[48;5;208m", ansiOrange, "DIVERGED"},
	kavach.StatusInvariantViolated: {"\033[48;5;201m", ansiMagenta, "INVARIANT VIOLATED"},
	kavach.StatusNondeterministic:  {"\033[48;5;220m", ansiYellow, "NONDETERMINISTIC"},
	kavach.StatusVariantFailed:     {"\033[48;5;196m", ansiRed, "VARIANT FAILED"},
	kavach.StatusUnverified:        {"\033[48;5;220m", ansiYellow, "UNVERIFIED"},
}

// verdict renders a verdict: a colored badge followed by the exact verdict
// string when color is on, the verdict string alone when it is off.
func (u ui) verdict(status kavach.Status, verdict string) string {
	st, ok := statusStyle[status]
	if !u.color || !ok {
		return verdict
	}
	badge := st.bg + ansiOnBlack + ansiBold + " " + st.text + " " + ansiReset
	return badge + " " + u.paint(verdict, ansiBold, st.fg)
}

// verdictColor returns the foreground color for a status.
func verdictColor(s kavach.Status) string {
	if st, ok := statusStyle[s]; ok {
		return st.fg
	}
	return ""
}

var typeColor = map[string]string{
	"input":    ansiBlue,
	"clock":    ansiDim,
	"rand":     ansiDim,
	"output":   ansiGreen,
	"marker":   ansiRed,
	"snapshot": ansiMagenta,
}
