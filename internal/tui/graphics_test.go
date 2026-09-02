package tui

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// Nothing in this repo can prove a terminal accepts these bytes, the way
// nothing but a real run proved an agent CLI's flags. What a test can hold is
// that the sequences do not drift, so the whole of each is pinned against a
// literal.
func TestKittyWritesTheSequencesItMeansTo(t *testing.T) {
	tests := []struct {
		name string
		call func(buf *bytes.Buffer) error
		want string
	}{
		{
			name: "transmit in one chunk",
			call: func(buf *bytes.Buffer) error {
				return kittyGraphics{}.Transmit(buf, 7, []byte("PNGDATA"))
			},
			want: "\x1b_Ga=t,f=100,t=d,i=7,q=2,m=0;" + base64.StdEncoding.EncodeToString([]byte("PNGDATA")) + "\x1b\\",
		},
		{
			name: "place",
			call: func(buf *bytes.Buffer) error {
				return kittyGraphics{}.Place(buf, 7, 60, 12)
			},
			want: "\x1b_Ga=p,i=7,p=7,c=60,r=12,C=1,q=2\x1b\\",
		},
		{
			name: "delete the placement",
			call: func(buf *bytes.Buffer) error {
				return kittyGraphics{}.Delete(buf, 7)
			},
			want: "\x1b_Ga=d,d=i,i=7,p=7,q=2\x1b\\",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := test.call(&buf); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := buf.String(); got != test.want {
				t.Errorf("wrote %q, want %q", got, test.want)
			}
		})
	}
}

// A screenshot is far past one chunk, and the protocol reads m=1 as "another
// follows". Getting the last chunk's m wrong leaves the terminal waiting for a
// continuation that never comes and nothing is ever drawn.
func TestKittyChunksALargeImage(t *testing.T) {
	// Three chunks' worth of base64, so there is a middle chunk to get wrong.
	data := bytes.Repeat([]byte("z"), kittyChunk*2)

	var buf bytes.Buffer
	if err := (kittyGraphics{}).Transmit(&buf, 3, data); err != nil {
		t.Fatalf("Transmit: %v", err)
	}

	sequences := strings.Split(strings.TrimSuffix(buf.String(), "\x1b\\"), "\x1b\\")
	if len(sequences) < 3 {
		t.Fatalf("%d sequences, want at least 3", len(sequences))
	}

	var payload strings.Builder
	for i, sequence := range sequences {
		control, chunk, found := strings.Cut(strings.TrimPrefix(sequence, "\x1b_G"), ";")
		if !found {
			t.Fatalf("sequence %d has no payload: %q", i, sequence)
		}
		payload.WriteString(chunk)

		if len(chunk) > kittyChunk {
			t.Errorf("sequence %d carries %d base64 characters, want at most %d", i, len(chunk), kittyChunk)
		}

		last := i == len(sequences)-1
		wantMore := "m=1"
		if last {
			wantMore = "m=0"
		}
		if !strings.HasSuffix(control, wantMore) {
			t.Errorf("sequence %d control = %q, want it to end in %s", i, control, wantMore)
		}

		// The keys ride on the first sequence only; a later one repeating them
		// restarts the transfer.
		if i == 0 && !strings.HasPrefix(control, "a=t,f=100,t=d,i=3,q=2") {
			t.Errorf("first control = %q, want the transmit keys", control)
		}
		if i > 0 && control != wantMore {
			t.Errorf("sequence %d control = %q, want only %s", i, control, wantMore)
		}
	}

	decoded, err := base64.StdEncoding.DecodeString(payload.String())
	if err != nil {
		t.Fatalf("the chunks do not rejoin into valid base64: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("rejoined %d bytes, want the %d sent", len(decoded), len(data))
	}
}

// The terminal scales a picture into exactly the box it is given, so a box that
// does not match the picture's own shape distorts it.
func TestImageBoxKeepsTheShapeAndTheCap(t *testing.T) {
	state := &graphicsState{cellWidth: 10, cellHeight: 20}

	tests := []struct {
		name          string
		maxCols       int
		width, height int
		wantCols      int
		wantRows      int
	}{
		{
			name: "a wide screenshot fills the measure",
			// 800x400 at 10x20 cells is 80 columns and 20 half-height rows,
			// so a 60-column measure asks for 30 pixels-worth: 15 rows.
			maxCols: 60, width: 800, height: 400, wantCols: 60, wantRows: 15,
		},
		{
			name:    "a tall picture is narrowed rather than squashed",
			maxCols: 60, width: 400, height: 800, wantCols: 15, wantRows: 15,
		},
		{
			name:    "a wide banner keeps at least one row",
			maxCols: 60, width: 2000, height: 20, wantCols: 60, wantRows: 1,
		},
		{
			name:    "no measure, no box",
			maxCols: 0, width: 800, height: 400, wantCols: 0, wantRows: 0,
		},
		{
			name:    "an unmeasurable picture has no box",
			maxCols: 60, width: 0, height: 0, wantCols: 0, wantRows: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cols, rows := state.imageBox(test.maxCols, test.width, test.height)
			if cols != test.wantCols || rows != test.wantRows {
				t.Errorf("imageBox(%d, %d, %d) = %dx%d, want %dx%d",
					test.maxCols, test.width, test.height, cols, rows, test.wantCols, test.wantRows)
			}
			if rows > maxImageRows {
				t.Errorf("rows = %d, past the cap of %d", rows, maxImageRows)
			}
		})
	}
}
