package tui

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"path"
	"regexp"
	"strings"

	"github.com/rivo/tview"

	"github.com/praxis-labs-io/zen-linear/internal/config"
	"github.com/praxis-labs-io/zen-linear/internal/images"
	"github.com/praxis-labs-io/zen-linear/internal/logger"
)

// A description's pictures are drawn where they sit. Glamour has no idea a
// terminal can draw one, so the image is taken out of the markdown before it
// runs and a sentinel is left in its place; the sentinel's line is then swapped
// for the rows the picture needs and a caption under them.
//
// Only an image standing alone on its own line is taken. Glamour word-wraps, so
// a sentinel inside a sentence could land anywhere on any line, and there is
// nothing to swap. One inside a sentence keeps the link glamour already draws,
// which is also the fallback for every picture that will not load.

// descriptionImagePattern matches an image standing alone on its line, which is
// how Linear writes an upload.
var descriptionImagePattern = regexp.MustCompile(`(?m)^[ \t]*!\[([^\]]*)\]\(([^)\s]+)\)[ \t]*$`)

// imageSentinel is what an image is replaced by before glamour runs. It is one
// unbroken token, so no wrap can split it, and it is rare enough that a real
// description cannot contain it by accident.
func imageSentinel(index int) string {
	return fmt.Sprintf("⟦zli-image-%d⟧", index)
}

// imageState is how far along one picture is.
type imageState int

const (
	imagePending imageState = iota
	imageReady
	imageFailed
)

// loadedImage is one picture the app knows about, kept by URL for as long as
// the app runs. The id is the terminal's handle on it, so it must not change
// while the terminal still holds the bytes.
type loadedImage struct {
	id    uint32
	state imageState
	image images.Image
}

// descriptionImage is one picture in the description being rendered.
type descriptionImage struct {
	url       string
	caption   string
	loaded    *loadedImage
	placement uint32
}

// imagesEnabled reports whether pictures are drawn at all. The store is built
// only where they would be, so its presence is the whole answer and the setting
// and the launch probe are read in one place: rebuildImageStore.
func (a *App) imagesEnabled() bool {
	return a.imageStore != nil
}

// rebuildImageStore points the store at the credentials in force. It is built
// only where it would be used: a terminal that cannot draw, or a reader who
// turned pictures off, has no reason to be given a cache directory.
//
// A store that will not build is not an error the reader is told about. The
// descriptions render the way they always have.
func (a *App) rebuildImageStore(token string, useBearer bool) {
	a.imageStore = nil
	a.imageCache = nil
	if a.config.Images != config.ImagesAuto || !KittyGraphicsSupported() {
		return
	}

	store, err := images.NewStore(images.Options{Token: token, UseBearer: useBearer})
	if err != nil {
		logger.Warning("tui.images: %v", err)
		return
	}
	a.imageStore = store
}

// describedImages pulls the pictures out of the markdown and returns what is
// left for glamour, with a sentinel where each taken picture was. A picture the
// app has not seen before starts loading here.
func (a *App) describedImages(markdown string) (string, []descriptionImage) {
	if !a.imagesEnabled() {
		return markdown, nil
	}

	var found []descriptionImage
	// One placement per drawing, counted per upload: a description carrying the
	// same picture twice shares the bytes and not the placement.
	drawings := map[string]uint32{}

	lines := strings.Split(markdown, "\n")
	fenced := false
	for i, line := range lines {
		// A picture inside a fence is part of the sample, and swapping it for
		// blank rows breaks the thing the fence exists to show verbatim.
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}

		parts := descriptionImagePattern.FindStringSubmatch(line)
		if parts == nil {
			continue
		}
		alt, url := parts[1], parts[2]

		loaded := a.loadImage(url)
		if loaded == nil || loaded.state == imageFailed {
			// Left as markdown, so glamour draws the name and the link exactly
			// as it does for a terminal that cannot show pictures at all.
			continue
		}

		drawings[url]++
		found = append(found, descriptionImage{
			url:       url,
			caption:   imageCaption(alt, url),
			loaded:    loaded,
			placement: drawings[url],
		})
		lines[i] = imageSentinel(len(found) - 1)
	}

	return strings.Join(lines, "\n"), found
}

// imageCaption is what is written under the picture: the alt text the author
// gave it, or the file it came from.
func imageCaption(alt, url string) string {
	if trimmed := strings.TrimSpace(alt); trimmed != "" {
		return trimmed
	}
	// The URL's own path, not the whole string: path.Base of a URL ending in a
	// slash answers with the host.
	if parsed, err := neturl.Parse(url); err == nil {
		if name := path.Base(parsed.Path); name != "" && name != "." && name != "/" {
			return name
		}
	}
	return "Image"
}

// loadImage returns what the app knows about a URL, starting a fetch the first
// time it is asked. Nil means pictures are off.
func (a *App) loadImage(url string) *loadedImage {
	if a.imageStore == nil {
		return nil
	}
	if a.imageCache == nil {
		a.imageCache = map[string]*loadedImage{}
	}
	if known, ok := a.imageCache[url]; ok {
		return known
	}

	a.imageIDs++
	loaded := &loadedImage{id: a.imageIDs, state: imagePending}
	a.imageCache[url] = loaded

	issueID := a.detailsIssueID
	store := a.imageStore
	go func() {
		fetched, err := store.Fetch(context.Background(), url)
		a.QueueUpdateDraw(func() {
			if err != nil {
				loaded.state = imageFailed
				// An off-host URL is not a failure worth a line in the log: it
				// is a link in a description, and most of them are.
				if !errors.Is(err, images.ErrUnsupportedHost) {
					logger.Debug("tui.images: fetch failed url=%s error=%v", url, err)
				}
			} else {
				loaded.state, loaded.image = imageReady, fetched
			}
			// The issue moved on while this was in flight, so the page being
			// drawn says nothing about this picture.
			if a.detailsIssueID != issueID {
				return
			}
			a.redrawDetailsBody()
		})
	}()

	return loaded
}

// redrawDetailsBody re-renders the description and the page around it at the
// width already fitted. refitDetailsPage cannot do this: it skips a width it
// has laid out at, and nothing about the width has changed.
func (a *App) redrawDetailsBody() {
	if a.detailsFittedWidth <= 0 {
		return
	}
	row, column := a.detailsPageView.GetScrollOffset()
	a.renderDetailsBody(a.detailsFittedWidth)
	a.renderDetailsPage()
	a.detailsPageView.ScrollTo(row, column)
}

// reserveImageRows swaps each sentinel's line for the rows its picture needs and
// a caption under them, and reports where those rows landed in the lines given.
//
// The rows are counted against lines, not held apart: every span and slot on the
// page is len(lines) where it is emitted, so a count kept elsewhere would have
// to be recomputed on every refit.
func (a *App) reserveImageRows(lines []string, found []descriptionImage, width int) ([]string, []pageImage) {
	if len(found) == 0 {
		return lines, nil
	}

	out := make([]string, 0, len(lines))
	placements := make([]pageImage, 0, len(found))
	for _, line := range lines {
		index, ok := sentinelIndex(line, len(found))
		if !ok {
			out = append(out, line)
			continue
		}

		image := found[index]
		if image.loaded.state == imageReady {
			cols, rows := a.graphics.imageBox(width, a.imageRowBudget(), image.loaded.image.Width, image.loaded.image.Height)
			if cols > 0 && rows > 0 {
				placements = append(placements, pageImage{
					id:        image.loaded.id,
					placement: image.placement,
					path:      image.loaded.image.Path,
					row:       len(out),
					rows:      rows,
					column:    0,
					cols:      cols,
				})
				for range rows {
					out = append(out, "")
				}
			}
			out = append(out, a.imageCaptionLine(image.caption, width))
			continue
		}

		out = append(out, a.imageCaptionLine("Loading "+image.caption+"…", width))
	}

	return out, placements
}

// imageRowBudget is the tallest a picture may be drawn here: the cap, and never
// more than the pane can show at once. visibleImages drops one that does not fit
// whole, so rows reserved past the pane's height would stay blank forever.
//
// Before the first draw the pane has no measured height, and the cap stands
// alone; the refit that follows re-lays the page against the real one.
func (a *App) imageRowBudget() int {
	budget := maxImageRows
	// The caption sits under the picture and the pane wants a row of its own
	// above the fold, so the height is not spent to the last cell.
	if fitted := a.detailsFittedHeight - 2; fitted > 0 && fitted < budget {
		budget = fitted
	}
	return budget
}

// imageCaptionLine is the line under a picture, in the shade a comment's byline
// takes: it names the picture rather than being part of the prose.
func (a *App) imageCaptionLine(caption string, width int) string {
	return fitTagged(a.themeTags.SecondaryText+tview.Escape(caption)+"[-]", width)
}

// sentinelIndex reports which picture a line stands for, if any.
func sentinelIndex(line string, count int) (int, bool) {
	for index := range count {
		if strings.Contains(line, imageSentinel(index)) {
			return index, true
		}
	}
	return 0, false
}
