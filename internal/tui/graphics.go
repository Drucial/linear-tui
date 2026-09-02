package tui

import (
	"encoding/base64"
	"fmt"
	"io"
)

// graphicsProtocol is how a picture reaches the terminal. Kitty is the only
// implementation; Sixel would be a second file and a second capability bit,
// which is the whole reason transmitting and placing are separate calls.
//
// They are separate for a second reason. A placement moves on every scroll, and
// re-sending the bytes each time would push a screenshot's worth of base64 down
// the tty per keypress.
type graphicsProtocol interface {
	// Transmit hands the terminal the file's bytes under an id, once.
	Transmit(w io.Writer, id uint32, data []byte) error
	// Place draws a transmitted image at the cursor, scaled into cols by rows.
	Place(w io.Writer, id uint32, cols, rows int) error
	// Delete removes an image's placement, keeping the bytes for the next one.
	Delete(w io.Writer, id uint32) error
}

// kittyChunk is the largest payload one escape sequence may carry, in base64
// characters. The protocol fixes it at 4096.
const kittyChunk = 4096

// kittyGraphics speaks the Kitty graphics protocol.
//
// Every command carries q=2, which tells the terminal to answer nothing. A
// reply would arrive on the tty tcell is reading and be delivered as keys.
type kittyGraphics struct{}

// Transmit sends the file verbatim (f=100) and stores it against id without
// drawing anything (a=t). The terminal decodes it, so no format is named and
// nothing here has to.
func (kittyGraphics) Transmit(w io.Writer, id uint32, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("kitty: image %d is empty", id)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	for first := true; len(encoded) > 0; first = false {
		chunk := encoded
		if len(chunk) > kittyChunk {
			chunk = chunk[:kittyChunk]
		}
		encoded = encoded[len(chunk):]

		// The keys ride on the first chunk only; every later one carries just
		// m, which says whether another follows.
		more := 0
		if len(encoded) > 0 {
			more = 1
		}
		control := fmt.Sprintf("m=%d", more)
		if first {
			control = fmt.Sprintf("a=t,f=100,t=d,i=%d,q=2,m=%d", id, more)
		}

		if _, err := fmt.Fprintf(w, "\x1b_G%s;%s\x1b\\", control, chunk); err != nil {
			return fmt.Errorf("kitty: transmit image %d: %w", id, err)
		}
	}
	return nil
}

// Place draws the stored image at the cursor, scaled into a cols by rows box.
// The terminal does the scaling, which is what spares this package a decoder
// and a resampler.
//
// C=1 leaves the cursor where it was. Without it the placement advances it and
// the next thing tcell writes lands somewhere it did not intend.
func (kittyGraphics) Place(w io.Writer, id uint32, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("kitty: image %d has no room: %dx%d", id, cols, rows)
	}
	// The placement id is the image id, since one image is drawn once.
	if _, err := fmt.Fprintf(w, "\x1b_Ga=p,i=%d,p=%d,c=%d,r=%d,C=1,q=2\x1b\\", id, id, cols, rows); err != nil {
		return fmt.Errorf("kitty: place image %d: %w", id, err)
	}
	return nil
}

// Delete removes the placement and leaves the transmitted bytes alone, so the
// next frame places the same image again without re-sending it. Freeing the
// data would need the uppercase d=I.
func (kittyGraphics) Delete(w io.Writer, id uint32) error {
	if _, err := fmt.Fprintf(w, "\x1b_Ga=d,d=i,i=%d,p=%d,q=2\x1b\\", id, id); err != nil {
		return fmt.Errorf("kitty: delete image %d: %w", id, err)
	}
	return nil
}
