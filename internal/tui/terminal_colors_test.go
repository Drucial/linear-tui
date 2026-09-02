package tui

import (
	"strings"
	"testing"
)

// TestParseKittyGraphicsReadsTheAnswer covers what the launch probe can be
// handed: a terminal that draws graphics, one that understands the protocol but
// refused this image, one that never answered, and a reply still arriving.
func TestParseKittyGraphicsReadsTheAnswer(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  bool
	}{
		{
			name:  "ok among the color reports",
			reply: "\x1b]10;rgb:e0e0/def4/f4f4\x1b\\\x1b]11;rgb:2323/2121/3030\x1b\\\x1b_Gi=31;OK\x1b\\\x1b[?62;c",
			want:  true,
		},
		{
			name:  "ok carrying other keys",
			reply: "\x1b_Gi=31,I=0;OK\x1b\\\x1b[?62;c",
			want:  true,
		},
		{
			name:  "an error is not support",
			reply: "\x1b_Gi=31;ENOTSUPPORTED:format\x1b\\\x1b[?62;c",
			want:  false,
		},
		{
			name:  "silence is not support",
			reply: "\x1b]10;rgb:ffff/ffff/ffff\x1b\\\x1b[?62;c",
			want:  false,
		},
		{
			name:  "an unterminated answer is not read short",
			reply: "\x1b_Gi=31;OK",
			want:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseKittyGraphics(test.reply); got != test.want {
				t.Errorf("parseKittyGraphics(%q) = %v, want %v", test.reply, got, test.want)
			}
		})
	}
}

// TestTerminalQueryEndsInDeviceAttributes pins the order the read loop depends
// on: the graphics question has to be asked before the query that closes the
// reply, or the loop stops before the answer arrives.
func TestTerminalQueryEndsInDeviceAttributes(t *testing.T) {
	graphics := strings.Index(terminalQuery, "a=q")
	if graphics < 0 {
		t.Fatalf("terminalQuery %q does not ask the graphics question", terminalQuery)
	}
	attributes := strings.LastIndex(terminalQuery, "\x1b[c")
	if attributes != len(terminalQuery)-len("\x1b[c") {
		t.Fatalf("terminalQuery does not end in the device attributes query: %q", terminalQuery)
	}
	if graphics > attributes {
		t.Error("the graphics question is asked after the query that closes the reply")
	}
	if !hasDeviceAttributes("\x1b[?62;c") {
		t.Error("hasDeviceAttributes does not match the reply the loop waits for")
	}
}
