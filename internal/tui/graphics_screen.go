package tui

import (
	"os"

	"github.com/gdamore/tcell/v2"
	"github.com/gdamore/tcell/v2/terminfo"
	"github.com/praxis-labs-io/zen-linear/internal/logger"
)

// The details page records where its pictures landed; this is what puts them on
// the terminal.
//
// It runs from Application.SetAfterDrawFunc, which tview calls once the
// primitives have drawn and before screen.Show flushes them. That is the only
// window that works: the graphic goes out first, then tcell writes the cells it
// has been told to skip.
//
// It writes to screen.Tty rather than os.Stdout, so the bytes are ordered
// against tcell's own output instead of racing it. The simulation screen has no
// tty, which is what makes every existing test see none of this.

// defaultCellPixels is the cell shape assumed before the terminal has been
// asked. Two rows to a column is close enough to hold an aspect ratio at the
// first frame, and the real numbers arrive on the frame after.
const (
	defaultCellWidth  = 8
	defaultCellHeight = 16
)

// placementKey names one drawing of one image. Both halves are needed: the
// same upload twice in a description shares its bytes and not its placement.
type placementKey struct {
	image     uint32
	placement uint32
}

func keyOf(image screenImage) placementKey {
	return placementKey{image: image.id, placement: image.placement}
}

// graphicsState is what the terminal is currently showing, so a frame that
// changed nothing writes nothing.
type graphicsState struct {
	protocol graphicsProtocol
	// sent is the images the terminal already holds bytes for, by id.
	sent map[uint32]bool
	// placed is where each placement is currently drawn.
	placed map[placementKey]screenImage
	// cellWidth and cellHeight are one cell in pixels, which is what turns an
	// image's own aspect ratio into a count of rows.
	cellWidth  int
	cellHeight int
	// screen is the one the last frame drew on, kept because tview hands the
	// screen to a draw handler and exposes it nowhere else, and a teardown that
	// runs outside a draw still has placements to take down.
	screen tcell.Screen
}

func newGraphicsState() *graphicsState {
	return &graphicsState{
		protocol:   kittyGraphics{},
		sent:       map[uint32]bool{},
		placed:     map[placementKey]screenImage{},
		cellWidth:  defaultCellWidth,
		cellHeight: defaultCellHeight,
	}
}

// beginImageFrame drops what the last frame asked for, before anything draws.
//
// A frame the details page does not draw at all is the case this exists for:
// the pane is unmounted by `v`, by the zoom, and by the medium and narrow
// layouts, and `contentFlex.Clear` means its Draw never runs. Left standing,
// the last frame's list is placed again over whatever now occupies those cells.
// Clearing here makes "nothing asked" the default, so every unmount path
// corrects itself rather than each one having to remember.
func (a *App) beginImageFrame() {
	a.pendingImages = nil
}

// imagesWanted is the pictures the terminal should be showing.
//
// A picture is a layer the terminal owns, above the cells rather than in them,
// so an overlay drawn after the details pane does not cover it — it came out
// over the settings modal. Nothing is placed while one is up, and the delete
// pass takes down whatever already was.
//
// The palette is asked for separately because it is not a modal in the
// dispatch sense: its page is added once and shown and hidden, where every
// entry in modalBindings is added by the modal that owns it. activeModal reads
// the page's presence, so it cannot see this one.
func (a *App) imagesWanted() []screenImage {
	if a.activeModal() != nil || a.paletteOpen() {
		return nil
	}
	return a.pendingImages
}

// quit takes the pictures down and stops the application.
//
// The teardown cannot wait until Run returns: Stop finalizes the screen, which
// closes the tty, so a delete written after it goes nowhere. Ghostty drops a
// placement when the alternate screen is left and hid this, but that is the
// terminal being tidy rather than the app being correct.
func (a *App) quit() {
	a.clearImages()
	a.app.Stop()
}

// recordImages takes the pictures a draw wants. The draw itself must not write
// to the tty: it runs under tcell's own lock, and the widgets have not flushed.
func (a *App) recordImages(images []screenImage) {
	a.pendingImages = images
}

// drawImages is the after-draw handler. Every failure is silent — a picture
// that would not draw is not worth a message, and the text under it still says
// what the description said.
func (a *App) drawImages(screen tcell.Screen) {
	tty, ok := screen.Tty()
	if !ok {
		// The simulation screen, so a test. Nothing to draw on.
		return
	}
	state := a.graphics
	if state == nil {
		return
	}
	state.screen = screen

	if size, err := tty.WindowSize(); err == nil {
		if width, height := size.CellDimensions(); width > 0 && height > 0 {
			state.cellWidth, state.cellHeight = width, height
		}
	}

	pending := a.imagesWanted()

	wanted := map[placementKey]screenImage{}
	for _, image := range pending {
		wanted[keyOf(image)] = image
	}

	// Gone or moved: the placement is dropped before anything new is drawn, so
	// two copies of one picture are never on screen at once.
	for key, was := range state.placed {
		if now, still := wanted[key]; still && now == was {
			continue
		}
		if err := state.protocol.Delete(tty, key.image, key.placement); err != nil {
			logger.Debug("tui.graphics: delete image id=%d placement=%d error=%v", key.image, key.placement, err)
		}
		delete(state.placed, key)
		// The cells under it go back to tcell, or the text that replaces the
		// picture is never painted.
		screen.LockRegion(was.x, was.y, was.cols, was.rows, false)
	}

	info, err := terminfo.LookupTerminfo(os.Getenv("TERM"))
	if err != nil {
		return
	}

	for _, image := range pending {
		if _, already := state.placed[keyOf(image)]; already {
			// Unmoved, so the terminal is still drawing it. The lock is set
			// again because a resize reallocates the cell buffer and drops it.
			screen.LockRegion(image.x, image.y, image.cols, image.rows, true)
			continue
		}
		if !state.sent[image.id] {
			data, err := os.ReadFile(image.path)
			if err != nil {
				logger.Debug("tui.graphics: read image path=%s error=%v", image.path, err)
				continue
			}
			if err := state.protocol.Transmit(tty, image.id, data); err != nil {
				logger.Debug("tui.graphics: transmit image id=%d error=%v", image.id, err)
				continue
			}
			state.sent[image.id] = true
		}

		info.TPuts(tty, info.TGoto(image.x, image.y))
		if err := state.protocol.Place(tty, image.id, image.placement, image.cols, image.rows); err != nil {
			logger.Debug("tui.graphics: place image id=%d error=%v", image.id, err)
			continue
		}
		state.placed[keyOf(image)] = image
		screen.LockRegion(image.x, image.y, image.cols, image.rows, true)
	}
}

// clearImages takes every placement off the terminal. Leaving the issue, an
// image turned off, and quitting all go through here: a placement nothing
// deletes is a picture stranded over whatever the terminal shows next.
func (a *App) clearImages() {
	state := a.graphics
	if state == nil || len(state.placed) == 0 {
		return
	}
	screen := state.screen
	if screen == nil {
		return
	}
	tty, ok := screen.Tty()
	if !ok {
		return
	}
	for key, was := range state.placed {
		if err := state.protocol.Delete(tty, key.image, key.placement); err != nil {
			logger.Debug("tui.graphics: delete image id=%d placement=%d error=%v", key.image, key.placement, err)
		}
		screen.LockRegion(was.x, was.y, was.cols, was.rows, false)
	}
	state.placed = map[placementKey]screenImage{}
	a.pendingImages = nil
}

// imageBox is the cell box an image of this shape draws in, given the widest it
// may be. It is what the description reserves rows for and what the placement
// scales into, so both read it from here.
//
// Past the row cap the box is narrowed rather than the rows clipped: the
// terminal scales the picture into exactly the box it is given, so keeping the
// full width and fewer rows would squash it.
//
// maxRows is clamped against the pane by the caller as well as the constant,
// the way the details chooser clamps its own row cap: visibleImages drops a
// picture that does not fit whole, so rows reserved past the pane's height are
// blank forever.
func (s *graphicsState) imageBox(maxCols, maxRows, width, height int) (cols, rows int) {
	if maxCols <= 0 || maxRows <= 0 || width <= 0 || height <= 0 || s.cellWidth <= 0 || s.cellHeight <= 0 {
		return 0, 0
	}

	cols = maxCols
	rows = ceilDiv(cols*s.cellWidth*height, width*s.cellHeight)
	if rows > maxRows {
		rows = maxRows
		cols = ceilDiv(rows*s.cellHeight*width, height*s.cellWidth)
		if cols > maxCols {
			cols = maxCols
		}
	}
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	return cols, rows
}

// ceilDiv divides and rounds up, so a picture is never given fewer cells than
// it covers and cut off by a rounding.
func ceilDiv(numerator, denominator int) int {
	if denominator <= 0 {
		return 0
	}
	return (numerator + denominator - 1) / denominator
}

const (
	// maxImageRows caps a picture's height so one screenshot cannot bury the
	// description it illustrates.
	maxImageRows = 15

	// minImageRows is the least a picture is worth drawing in. Under it the
	// description keeps the link instead: a few rows show nothing anyone can
	// read, and a picture the pane cannot fit whole is never placed at all.
	minImageRows = 6
)
