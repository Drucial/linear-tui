//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package tui

// queryTerminal has no answer off unix: the query needs a controlling tty to
// read from and a raw mode to read it in.
func queryTerminal() terminalReply {
	return terminalReply{}
}
