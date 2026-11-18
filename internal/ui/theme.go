package ui

import "github.com/charmbracelet/lipgloss"

// Catppuccin Mocha, the same default skin sofka uses.
var (
	cRed      = lipgloss.Color("#f38ba8")
	cPeach    = lipgloss.Color("#fab387")
	cYellow   = lipgloss.Color("#f9e2af")
	cGreen    = lipgloss.Color("#a6e3a1")
	cTeal     = lipgloss.Color("#94e2d5")
	cSky      = lipgloss.Color("#89dceb")
	cBlue     = lipgloss.Color("#89b4fa")
	cLavender = lipgloss.Color("#b4befe")
	cMauve    = lipgloss.Color("#cba6f7")
	cPink     = lipgloss.Color("#f5c2e7")
	cText     = lipgloss.Color("#cdd6f4")
	cSubtext0 = lipgloss.Color("#a6adc8")
	cOverlay2 = lipgloss.Color("#9399b2")
	cOverlay1 = lipgloss.Color("#7f849c")
	cOverlay0 = lipgloss.Color("#6c7086")
	cSurface1 = lipgloss.Color("#45475a")
	cBase     = lipgloss.Color("#1e1e2e")
)

func fg(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

var (
	stHeaderBorder = fg(cMauve)
	stTableBorder  = fg(cLavender)
	stBrand        = fg(cGreen).Bold(true)
	stLabel        = fg(cOverlay2)
	stContext      = fg(cMauve)
	stCluster      = fg(cSky)
	stAtespace     = fg(cGreen)
	stResource     = fg(cPeach)
	stCount        = fg(cText)
	stReadOnly     = fg(cRed)
	stWrite        = fg(cPeach)
	stHintKey      = fg(cTeal).Bold(true)
	stHintDesc     = fg(cOverlay2)
	stLogo         = fg(cMauve)
	stLogoEye      = fg(cRed)
	stVersion      = fg(cOverlay2)
	stTitleKind    = fg(cGreen).Bold(true)
	stTitleCount   = fg(cPeach)
	stTitleFilter  = fg(cYellow)
	stColHeader    = fg(cYellow)
	stSelected     = lipgloss.NewStyle().Background(cLavender).Foreground(cBase).Bold(true)
	stFooterHint   = fg(cOverlay2)
	stStatus       = fg(cText)
	stStatusErr    = fg(cRed)
	stLive         = fg(cGreen)
	stPaused       = fg(cPeach)
	stDead         = fg(cRed)
	stInputPrompt  = fg(cPeach).Bold(true)
	stInput        = fg(cText)
	stGhost        = fg(cOverlay0)
	stSuggest      = fg(cOverlay2)
	stSuggestSel   = fg(cPeach).Bold(true)
	stYAMLKey      = fg(cBlue)
	stYAMLVal      = fg(cText)
	stYAMLDash     = fg(cOverlay1)
	stSection      = fg(cMauve).Bold(true)
	stDim          = fg(cOverlay1)
	stLogContainer = fg(cTeal)
	stLogLifecycle = fg(cMauve)
	stLogTime      = fg(cOverlay1)
	stMark         = fg(cPink)
)

// rowLevel picks a row's color, mirroring sofka: healthy rows blue, errors
// red, in-progress peach, idle grey.
type rowLevel int

const (
	lvNormal rowLevel = iota
	lvIdle
	lvProgress
	lvWarn
	lvError
	lvGone
)

var rowStyles = map[rowLevel]lipgloss.Style{
	lvNormal:   fg(cBlue),
	lvIdle:     fg(cSubtext0),
	lvProgress: fg(cPeach),
	lvWarn:     fg(cYellow),
	lvError:    fg(cRed),
	lvGone:     fg(cOverlay0),
}
