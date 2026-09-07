// Package images fetches the pictures an issue's description points at and
// keeps them on disk, so the details pane has bytes to draw and dimensions to
// reserve rows for.
//
// It is separate from internal/linearapi on purpose. That client's transport is
// wrapped for GraphQL — retries, a rate-limit budget, a replayable context —
// and none of it means anything for a file download.
package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// The decoder for the one format kept. Registration is process-wide and
	// another package importing image/jpeg would quietly widen what decodes
	// here, so the format is checked by name below rather than by what happens
	// to be registered.
	_ "image/png"

	"github.com/praxis-labs-io/zen-linear/internal/config"
)

const (
	// requestTimeout bounds a single fetch. A screenshot is not worth holding
	// a details pane open for.
	requestTimeout = 20 * time.Second

	// maxBytes is the largest image the store will keep. Linear caps an upload
	// well below this; the limit is here so a wrong URL cannot fill a disk.
	maxBytes = 10 << 20

	// defaultMaxAge is how long a cached image is kept. It is a cache, so an
	// old entry is discarded rather than revalidated.
	defaultMaxAge = 30 * 24 * time.Hour

	// dirName is the store's directory under the application directory.
	dirName = "images"
)

// uploadHost is the only host the Linear token is ever sent to. A description
// can point at any URL on the internet, and attaching a live credential to a
// request for one would hand it over.
const uploadHost = "uploads.linear.app"

// ErrUnsupportedHost is returned for a URL the store will not fetch. The caller
// leaves that image as the link it already renders.
var ErrUnsupportedHost = errors.New("images: not a Linear upload")

// keptFormat is the only format stored. PNG is the one encoded format the Kitty
// graphics protocol accepts (f=100; its others are raw pixels), and a picture
// the terminal cannot be handed must not be measured: rows would be reserved
// for it and the link it replaced is already gone, while q=2 means the
// terminal's refusal never comes back to say so.
const keptFormat = "png"

// Image is a picture on disk and the size it draws at.
type Image struct {
	// Path is the file holding the bytes, ready to be sent to the terminal.
	Path string
	// Width and Height are the image's own pixels.
	Width  int
	Height int
}

// Options is what a store needs. Only Token is required; Client, CacheDir, Now
// and MaxAge exist so tests never reach Linear or a real home directory.
type Options struct {
	// Token authenticates the download. An upload URL answers 401 without it.
	Token string
	// UseBearer prefixes the token with "Bearer " (OAuth). A personal API key
	// leaves this false, the way linearapi.ClientConfig does.
	UseBearer bool
	// Host overrides the only host the token is sent to. Empty means the real
	// upload host.
	Host string
	// CacheDir overrides where images are kept. Empty means the application
	// directory's own.
	CacheDir string
	// Client overrides the HTTP client. Nil means one bounded by requestTimeout.
	Client *http.Client
	// MaxAge overrides how long an entry is kept. Zero means defaultMaxAge.
	MaxAge time.Duration
	// Now overrides the clock, so a test can age an entry without waiting.
	Now func() time.Time
}

// Store fetches images and keeps them under one directory.
type Store struct {
	token     string
	useBearer bool
	host      string
	dir       string
	client    *http.Client
}

// NewStore prepares the cache directory and prunes what has aged out of it. A
// directory it cannot create is an error: without one there is nowhere to put
// the bytes the terminal is handed.
func NewStore(opts Options) (*Store, error) {
	dir := opts.CacheDir
	if dir == "" {
		base, err := config.Dir()
		if err != nil {
			return nil, fmt.Errorf("resolve image cache directory: %w", err)
		}
		dir = filepath.Join(base, dirName)
	}

	// config.EnsureDirFor tightens only the application directory itself, and
	// this is a directory under it holding one workspace's private pictures, so
	// the mode is set here. A chmod refusal is not fatal, for the reason given
	// there: not every filesystem a home directory sits on implements it.
	if err := os.MkdirAll(dir, config.DirMode); err != nil {
		return nil, fmt.Errorf("create image cache directory: %w", err)
	}
	_ = os.Chmod(dir, config.DirMode)

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	host := opts.Host
	if host == "" {
		host = uploadHost
	}

	store := &Store{
		token:     opts.Token,
		useBearer: opts.UseBearer,
		host:      host,
		dir:       dir,
		client:    client,
	}

	maxAge := opts.MaxAge
	if maxAge == 0 {
		maxAge = defaultMaxAge
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	store.prune(now(), maxAge)

	return store, nil
}

// Fetch returns the image at raw, from the cache when it is there and from
// Linear when it is not.
func (s *Store) Fetch(ctx context.Context, raw string) (Image, error) {
	if err := s.allowed(raw); err != nil {
		return Image{}, err
	}

	path := filepath.Join(s.dir, cacheName(raw))
	if img, ok := s.cached(path); ok {
		return img, nil
	}

	data, err := s.download(ctx, raw)
	if err != nil {
		return Image{}, err
	}

	header, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("images: read %s: %w", raw, err)
	}
	if format != keptFormat {
		return Image{}, fmt.Errorf("images: %s is %s, and only %s can be drawn", raw, format, keptFormat)
	}

	if err := writeCache(path, data); err != nil {
		return Image{}, err
	}

	return Image{Path: path, Width: header.Width, Height: header.Height}, nil
}

// allowed reports whether the store will attach the token to this URL.
func (s *Store) allowed(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnsupportedHost, raw)
	}
	if parsed.Scheme != "https" || parsed.Hostname() != s.host {
		return fmt.Errorf("%w: %s", ErrUnsupportedHost, raw)
	}
	return nil
}

// download asks Linear for the bytes, bounded in both time and size.
func (s *Store) download(ctx context.Context, raw string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("images: request %s: %w", raw, err)
	}
	if s.token != "" {
		token := s.token
		if s.useBearer {
			token = "Bearer " + token
		}
		req.Header.Set("Authorization", token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("images: fetch %s: %w", raw, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("images: fetch %s: %s", raw, resp.Status)
	}

	// One byte past the cap, so a file at exactly the limit still reads whole
	// and anything larger is refused rather than silently truncated.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("images: read %s: %w", raw, err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("images: %s is larger than %d bytes", raw, maxBytes)
	}

	return data, nil
}

// cached reads an entry back. Its dimensions are re-read from the file's own
// header rather than an index beside it: DecodeConfig stops at the header, and
// an index is a second file to keep in step with the first.
func (s *Store) cached(path string) (Image, bool) {
	file, err := os.Open(path)
	if err != nil {
		return Image{}, false
	}
	defer func() { _ = file.Close() }()

	header, format, err := image.DecodeConfig(file)
	if err != nil || format != keptFormat {
		return Image{}, false
	}
	return Image{Path: path, Width: header.Width, Height: header.Height}, true
}

// prune drops entries that have aged out. Every failure is ignored: a cache
// that could not be tidied still answers.
func (s *Store) prune(now time.Time, maxAge time.Duration) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
}

// cacheName is the file an URL is kept under. The hash is what keeps a name
// out of the path: an upload URL carries workspace and issue ids.
func cacheName(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// writeCache stores the bytes, at the mode every other file this app writes
// under the application directory uses.
func writeCache(path string, data []byte) error {
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("images: cache %s: %w", path, err)
	}
	return nil
}
