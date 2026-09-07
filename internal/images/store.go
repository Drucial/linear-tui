// Package images fetches the pictures an issue's description points at and
// keeps them on disk, so the details pane has bytes to draw and dimensions to
// reserve rows for.
//
// Separate from internal/linearapi, whose transport is wrapped for GraphQL.
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

	// Registration is process-wide, so the format is checked by name below
	// rather than by whatever happens to be registered.
	_ "image/png"

	"github.com/praxis-labs-io/zen-linear/internal/config"
)

const (
	requestTimeout = 20 * time.Second
	// So a wrong URL cannot fill a disk. Linear caps an upload well below it.
	maxBytes = 10 << 20
	// A cache, so an old entry is discarded rather than revalidated.
	defaultMaxAge = 30 * 24 * time.Hour
	dirName       = "images"
)

// The only host the Linear token is ever sent to. A description can point at
// any URL on the internet, and a live credential on a request for one would
// hand it over.
const uploadHost = "uploads.linear.app"

// The caller leaves an image it names as the link it already renders.
var ErrUnsupportedHost = errors.New("images: not a Linear upload")

// The one encoded format Kitty accepts (f=100; its others are raw pixels). A
// picture the terminal cannot be handed must not be measured: rows would be
// reserved for it, its link is already gone, and q=2 means the terminal's
// refusal never comes back to say so.
const keptFormat = "png"

// A picture on disk and the pixels it draws at.
type Image struct {
	Path   string
	Width  int
	Height int
}

// Only Token is required. Host, CacheDir, Client, MaxAge and Now exist so tests
// never reach Linear or a real home directory.
type Options struct {
	// An upload URL answers 401 without it.
	Token string
	// OAuth prefixes the token with "Bearer "; a personal API key does not.
	UseBearer bool
	Host      string
	CacheDir  string
	Client    *http.Client
	MaxAge    time.Duration
	Now       func() time.Time
}

type Store struct {
	token     string
	useBearer bool
	host      string
	dir       string
	client    *http.Client
}

// A directory it cannot create is an error: there is nowhere to put the bytes
// the terminal is handed.
func NewStore(opts Options) (*Store, error) {
	dir := opts.CacheDir
	if dir == "" {
		base, err := config.Dir()
		if err != nil {
			return nil, fmt.Errorf("resolve image cache directory: %w", err)
		}
		dir = filepath.Join(base, dirName)
	}

	// EnsureDirFor tightens only the application directory itself, and this one
	// under it holds a workspace's private pictures. A chmod refusal is not
	// fatal: not every filesystem a home directory sits on implements it.
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

// Whether the store will attach the token to this URL.
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

	// One past the cap, so a file at the limit reads whole and anything larger
	// is refused rather than truncated.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("images: read %s: %w", raw, err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("images: %s is larger than %d bytes", raw, maxBytes)
	}

	return data, nil
}

// Dimensions are re-read from the file's header rather than an index beside it:
// DecodeConfig stops at the header, and an index is a second file to keep in
// step with the first.
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

// Failures are ignored: a cache that could not be tidied still answers.
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

// Hashed because an upload URL carries workspace and issue ids.
func cacheName(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func writeCache(path string, data []byte) error {
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("images: cache %s: %w", path, err)
	}
	return nil
}
