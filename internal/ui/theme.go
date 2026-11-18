package ui

import (
	"github.com/ate-tooling/ate-tui/internal/explain"
	"github.com/ate-tooling/ate-tui/internal/skin"
	"github.com/charmbracelet/lipgloss"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// The palette and styles are set by ApplySkin. init applies the default,
// Catppuccin Mocha, the same skin sofka uses.
var (
	cRed, cPeach, cYellow, cGreen, cTeal, cSky, cBlue, cLavender, cMauve, cPink lipgloss.Color
	cText, cSubtext0, cOverlay2, cOverlay1, cOverlay0, cSurface1, cBase         lipgloss.Color
)

func init() { ApplySkin(skin.Builtin[skin.Default]) }

func fg(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

var (
	stHeaderBorder,
	stTableBorder,
	stBrand,
	stLabel,
	stContext,
	stCluster,
	stAtespace,
	stResource,
	stCount,
	stReadOnly,
	stWrite,
	stHintKey,
	stHintDesc,
	stLogo,
	stLogoEye,
	stVersion,
	stTitleKind,
	stTitleCount,
	stTitleFilter,
	stColHeader,
	stSelected,
	stFooterHint,
	stStatus,
	stStatusErr,
	stLive,
	stPaused,
	stDead,
	stInputPrompt,
	stInput,
	stGhost,
	stSuggest,
	stSuggestSel,
	stYAMLKey,
	stYAMLVal,
	stYAMLDash,
	stSection,
	stDim,
	stLogContainer,
	stLogLifecycle,
	stLogTime,
	stMark,
	stMatch lipgloss.Style
)

// ApplySkin sets the palette and rebuilds every style from it. main calls it
// once, before the program starts.
func ApplySkin(p skin.Palette) {
	cRed, cPeach, cYellow, cGreen, cTeal = lipgloss.Color(p.Red), lipgloss.Color(p.Peach), lipgloss.Color(p.Yellow), lipgloss.Color(p.Green), lipgloss.Color(p.Teal)
	cSky, cBlue, cLavender, cMauve, cPink = lipgloss.Color(p.Sky), lipgloss.Color(p.Blue), lipgloss.Color(p.Lavender), lipgloss.Color(p.Mauve), lipgloss.Color(p.Pink)
	cText, cSubtext0, cOverlay2, cOverlay1 = lipgloss.Color(p.Text), lipgloss.Color(p.Subtext0), lipgloss.Color(p.Overlay2), lipgloss.Color(p.Overlay1)
	cOverlay0, cSurface1, cBase = lipgloss.Color(p.Overlay0), lipgloss.Color(p.Surface1), lipgloss.Color(p.Base)

	stHeaderBorder = fg(cMauve)
	stTableBorder = fg(cLavender)
	stBrand = fg(cGreen).Bold(true)
	stLabel = fg(cOverlay2)
	stContext = fg(cMauve)
	stCluster = fg(cSky)
	stAtespace = fg(cGreen)
	stResource = fg(cPeach)
	stCount = fg(cText)
	stReadOnly = fg(cRed)
	stWrite = fg(cPeach)
	stHintKey = fg(cTeal).Bold(true)
	stHintDesc = fg(cOverlay2)
	stLogo = fg(cMauve)
	stLogoEye = fg(cRed)
	stVersion = fg(cOverlay2)
	stTitleKind = fg(cGreen).Bold(true)
	stTitleCount = fg(cPeach)
	stTitleFilter = fg(cYellow)
	stColHeader = fg(cYellow)
	stSelected = lipgloss.NewStyle().Background(cLavender).Foreground(cBase).Bold(true)
	stFooterHint = fg(cOverlay2)
	stStatus = fg(cText)
	stStatusErr = fg(cRed)
	stLive = fg(cGreen)
	stPaused = fg(cPeach)
	stDead = fg(cRed)
	stInputPrompt = fg(cPeach).Bold(true)
	stInput = fg(cText)
	stGhost = fg(cOverlay0)
	stSuggest = fg(cOverlay2)
	stSuggestSel = fg(cPeach).Bold(true)
	stYAMLKey = fg(cBlue)
	stYAMLVal = fg(cText)
	stYAMLDash = fg(cOverlay1)
	stSection = fg(cMauve).Bold(true)
	stDim = fg(cOverlay1)
	stLogContainer = fg(cTeal)
	stLogLifecycle = fg(cMauve)
	stLogTime = fg(cOverlay1)
	stMark = fg(cPink)
	stMatch = lipgloss.NewStyle().Background(cYellow).Foreground(cBase)

	rowStyles = map[rowLevel]lipgloss.Style{
		lvNormal:   fg(cBlue),
		lvIdle:     fg(cSubtext0),
		lvProgress: fg(cPeach),
		lvWarn:     fg(cYellow),
		lvError:    fg(cRed),
		lvGone:     fg(cOverlay0),
	}
	levelGlyph = map[explain.Level]string{
		explain.OK:    fg(cGreen).Render("✔"),
		explain.Info:  fg(cSky).Render("•"),
		explain.Warn:  fg(cPeach).Render("▲"),
		explain.Error: fg(cRed).Render("✖"),
	}
	levelStyle = map[explain.Level]func(...string) string{
		explain.OK:    fg(cGreen).Render,
		explain.Info:  fg(cText).Render,
		explain.Warn:  fg(cPeach).Render,
		explain.Error: fg(cRed).Render,
	}
	stateStyle = map[ateapi.ActorState]lipgloss.Style{
		ateapi.ActorState_ACTOR_STATE_RUNNING:    fg(cGreen),
		ateapi.ActorState_ACTOR_STATE_SUSPENDED:  fg(cSubtext0),
		ateapi.ActorState_ACTOR_STATE_RESUMING:   fg(cPeach),
		ateapi.ActorState_ACTOR_STATE_SUSPENDING: fg(cPeach),
		ateapi.ActorState_ACTOR_STATE_REVERTING:  fg(cPeach),
		ateapi.ActorState_ACTOR_STATE_CRASHED:    fg(cRed).Bold(true),
		ateapi.ActorState_ACTOR_STATE_DELETING:   fg(cOverlay0),
	}
}

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

var rowStyles map[rowLevel]lipgloss.Style
