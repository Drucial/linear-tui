//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package tui

import (
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// queryTerminal asks the terminal about itself: its own background and
// foreground, and whether it draws Kitty graphics. It goes raw, so it must run
// before tcell's own.
func queryTerminal() terminalReply {
	termName := os.Getenv("TERM")
	if termName == "" || strings.HasPrefix(termName, "dumb") {
		return terminalReply{}
	}

	// /dev/tty rather than stdin, so a piped or redirected stream still reaches
	// the terminal the user is sitting at.
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return terminalReply{}
	}
	defer func() { _ = unix.Close(fd) }()

	if !term.IsTerminal(fd) {
		return terminalReply{}
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return terminalReply{}
	}
	defer func() { _ = term.Restore(fd, state) }()

	if _, err := unix.Write(fd, []byte(terminalQuery)); err != nil {
		return terminalReply{}
	}

	deadline := time.Now().Add(terminalQueryTimeout)
	chunk := make([]byte, 256)
	var reply strings.Builder
	// The reply is read to its end even once the colors are in. Whatever is
	// left in the buffer is echoed and read as keys the moment raw mode goes.
	for waitForTTYData(fd, deadline) {
		read, err := unix.Read(fd, chunk)
		if read > 0 {
			reply.Write(chunk[:read])
		}
		if err != nil {
			break
		}
		if hasDeviceAttributes(reply.String()) {
			break
		}
	}
	answer := reply.String()
	background, foreground, colorsKnown := parseTerminalColors(answer)
	return terminalReply{
		background:    background,
		foreground:    foreground,
		colorsKnown:   colorsKnown,
		kittyGraphics: parseKittyGraphics(answer),
	}
}

// waitForTTYData reports whether the terminal answered before the deadline. The
// set and the timeval are rebuilt per call: select leaves both undefined when
// it returns an error, EINTR included.
func waitForTTYData(fd int, deadline time.Time) bool {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timeval := unix.NsecToTimeval(int64(remaining))
		var readable unix.FdSet
		readable.Set(fd)
		ready, err := unix.Select(fd+1, &readable, nil, nil, &timeval)
		if err == unix.EINTR {
			continue
		}
		return err == nil && ready > 0
	}
}
