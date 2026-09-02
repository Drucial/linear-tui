package tui

import (
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/gdamore/tcell/v2"
)

// terminalQueryTimeout is how long the OSC query waits for an answer. It is
// paid once at launch by terminals that ignore the query, so it is short.
const terminalQueryTimeout = 200 * time.Millisecond

// terminalQuery asks for the foreground (OSC 10), the background (OSC 11) and
// whether the terminal draws Kitty graphics, then for device attributes, which
// a terminal answers last and always. One probe, because each costs the
// timeout above on a terminal that stays silent.
//
// The graphics question is a=q against a 1x1 RGB pixel sent inline (t=d, f=24),
// which is the smallest thing a terminal can be asked to accept.
const terminalQuery = "\x1b]10;?\x1b\\\x1b]11;?\x1b\\\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\\x1b[c"

// oscColorReport matches a terminal's answer: an OSC code, then components of
// one to four hex digits each. The terminator is required, or a report still
// arriving matches short and a component scales to the wrong value.
var oscColorReport = regexp.MustCompile(`\x1b\]([0-9]{1,2});rgba?:([0-9a-fA-F]{1,4})/([0-9a-fA-F]{1,4})/([0-9a-fA-F]{1,4})(?:\x07|\x1b\\)`)

// deviceAttributesReport is the answer to the query that closes the reply. It
// arrives after the color reports, so seeing it means none are coming.
var deviceAttributesReport = regexp.MustCompile(`\x1b\[\?[0-9;]*c`)

// kittyGraphicsReport matches the answer to the graphics question. A terminal
// that draws them answers OK; one that understands the protocol but refused
// this image answers an error code, and one that does not understand it at all
// says nothing. Only OK is support.
var kittyGraphicsReport = regexp.MustCompile(`\x1b_Gi=31(?:,[^;]*)?;OK\x1b\\`)

// terminalReply is what the launch probe learned about the terminal.
type terminalReply struct {
	background    tcell.Color
	foreground    tcell.Color
	colorsKnown   bool
	kittyGraphics bool
}

// hasDeviceAttributes reports whether the terminal has finished answering.
func hasDeviceAttributes(reply string) bool {
	return deviceAttributesReport.MatchString(reply)
}

// parseKittyGraphics reports whether the terminal answered that it draws Kitty
// graphics.
func parseKittyGraphics(reply string) bool {
	return kittyGraphicsReport.MatchString(reply)
}

// parseTerminalColors reads the foreground and background out of whatever the
// terminal has sent so far. It reports false until both have arrived.
func parseTerminalColors(reply string) (background, foreground tcell.Color, ok bool) {
	var hasBackground, hasForeground bool
	for _, match := range oscColorReport.FindAllStringSubmatch(reply, -1) {
		color := tcell.NewRGBColor(
			scaleHexComponent(match[2]),
			scaleHexComponent(match[3]),
			scaleHexComponent(match[4]),
		)
		switch match[1] {
		case "10":
			foreground, hasForeground = color, true
		case "11":
			background, hasBackground = color, true
		}
	}
	return background, foreground, hasBackground && hasForeground
}

// scaleHexComponent widens a component of any width to 8 bits, since terminals
// answer in anything from rgb:1/2/3 to rgb:1c1c/1c1c/1c1c.
func scaleHexComponent(component string) int32 {
	value, err := strconv.ParseUint(component, 16, 32)
	if err != nil {
		return 0
	}
	full := float64(int64(1)<<(4*len(component)) - 1)
	return int32(math.Round(float64(value) / full * 255))
}
