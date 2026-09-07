package tui

import (
	"os"

	"github.com/gdamore/tcell/v2"
	"github.com/gdamore/tcell/v2/terminfo"
	"github.com/praxis-labs-io/zen-linear/internal/logger"
)

// The details page records where its pictures landed; this puts them on the
// terminal, from SetAfterDrawFunc. That is the only window that works: tview
// calls it once the primitives have drawn and before screen.Show, so the
// graphic goes out first and tcell then writes the cells it was told to skip.
//
// It writes to screen.Tty rather than os.Stdout, so the bytes are ordered
// against tcell's own output instead of racing it.

// The cell shape assumed before the terminal has been asked. The real numbers
// arrive on the frame after.
const (
	defaultCellWidth  = 8
	defaultCellHeight = 16
)

// Both halves are needed: the same upload twice in a description shares its
// bytes and not its placement.
type placementKey struct {
	image     uint32
	placement uint32
}

func keyOf(image screenImage) placementKey {
	return placementKey{image: image.id, placement: image.placement}
}

type graphicsState struct {
	protocol graphicsProtocol
	sent     map[uint32]bool
	placed   map[placementKey]screenImage
	// One cell in pixels, which turns an image's aspect ratio into rows.
	cellWidth  int
	cellHeight int
	// Kept because tview hands the screen to a draw handler and exposes it
	// nowhere else, and a teardown outside a draw still has placements to drop.
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

// Cleared before anything draws, so a frame where the details page does not
// draw at all asks for nothing rather than re-placing the last frame's list.
// Every unmount path goes through contentFlex.Clear and so corrects itself.
func (a *App) beginImageFrame() {
	a.pendingImages = nil
}

// A picture is a layer above the cells, so an overlay drawn after the details
// pane does not cover it. The palette is asked for separately: its page is
// added once and shown and hidden, so activeModal cannot see it.
func (a *App) imagesWanted() []screenImage {
	if a.activeModal() != nil || a.paletteOpen() {
		return nil
	}
	return a.pendingImages
}

// The teardown cannot wait until Run returns: Stop finalizes the screen, which
// closes the tty, so a delete written after it goes nowhere.
func (a *App) quit() {
	a.clearImages()
	a.app.Stop()
}

// The draw itself must not write to the tty: it runs under tcell's own lock,
// and the widgets have not flushed.
func (a *App) recordImages(images []screenImage) {
	a.pendingImages = images
}

// Every failure is silent: the text under a picture still says what the
// description said.
func (a *App) drawImages(screen tcell.Screen) {
	// No tty means the simulation screen, so every test sees none of this.
	tty, ok := screen.Tty()
	if !ok {
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

	// Gone or moved, dropped before anything new is drawn so two copies of one
	// picture are never on screen at once.
	for key, was := range state.placed {
		if now, still := wanted[key]; still && now == was {
			continue
		}
		if err := state.protocol.Delete(tty, key.image, key.placement); err != nil {
			logger.Debug("tui.graphics: delete image id=%d placement=%d error=%v", key.image, key.placement, err)
		}
		delete(state.placed, key)
		// The cells go back to tcell, or the text replacing the picture is
		// never painted.
		screen.LockRegion(was.x, was.y, was.cols, was.rows, false)
	}

	info, err := terminfo.LookupTerminfo(os.Getenv("TERM"))
	if err != nil {
		return
	}

	for _, image := range pending {
		if _, already := state.placed[keyOf(image)]; already {
			// Locked again because a resize reallocates the cell buffer and
			// drops every lock on it.
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

// A placement nothing deletes is a picture stranded over whatever the terminal
// shows next.
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

// The box the description reserves rows for and the placement scales into, so
// both read it from here.
//
// Past the cap the box is narrowed rather than the rows clipped: the terminal
// scales into exactly the box it is given, so fewer rows at full width squash
// the picture.
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

// Rounds up, so a picture is never given fewer cells than it covers.
func ceilDiv(numerator, denominator int) int {
	if denominator <= 0 {
		return 0
	}
	return (numerator + denominator - 1) / denominator
}

const (
	// So one screenshot cannot bury the description it illustrates.
	maxImageRows = 15

	// Under this the description keeps the link: a picture the pane cannot fit
	// whole is never placed, and reserved rows would stay blank.
	minImageRows = 6
)
