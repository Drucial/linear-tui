package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	// InstallScriptURL is the published installer, the same one the docs tell a
	// reader to pipe into sh.
	InstallScriptURL = "https://raw.githubusercontent.com/praxis-labs-io/zen-linear/main/install.sh"

	// InstallScriptWindowsURL is install.sh for Windows. It follows the shell
	// script's decisions rather than making its own, INSTALL_DIR included.
	InstallScriptWindowsURL = "https://raw.githubusercontent.com/praxis-labs-io/zen-linear/main/install.ps1"

	// DevVersion is what a build the release workflow never stamped reports.
	DevVersion = devVersion

	// maxScriptBytes caps what is read off the wire. Both installers are a few
	// kilobytes.
	maxScriptBytes = 1 << 20
)

// InstallRunner executes the staged installer. Tests replace it rather than
// running a shell.
type InstallRunner func(ctx context.Context, script, dir string, out io.Writer) error

// InstallOptions is what an install needs. Only Dir is required.
type InstallOptions struct {
	// Dir is where the binary lands, passed to the script as INSTALL_DIR.
	Dir string
	// Out receives the installer's own output. Nil discards it.
	Out io.Writer
	// ScriptURL overrides the installer's address. Zero means the one for this
	// platform.
	ScriptURL string
	// Client overrides the HTTP client used to fetch the script.
	Client *http.Client
	// Runner overrides how the staged script is executed.
	Runner InstallRunner
}

// Install fetches the published installer and runs it with INSTALL_DIR set to
// Dir. The script owns the platform matrix, the checksum gate and the replace
// of a running binary; nothing here duplicates any of that.
//
// The script is staged to a file rather than piped into a shell because a
// pipeline reports the shell's status: a download that failed would reach sh
// as empty input and exit 0, and the upgrade would be called a success.
func Install(ctx context.Context, opts InstallOptions) error {
	if opts.Dir == "" {
		return errors.New("install directory is empty")
	}

	script, err := fetchInstallScript(ctx, opts)
	if err != nil {
		return err
	}

	path, cleanup, err := stageScript(runtime.GOOS, script)
	if err != nil {
		return err
	}
	defer cleanup()

	run := opts.Runner
	if run == nil {
		run = runInstallScript
	}

	return run(ctx, path, opts.Dir, opts.Out)
}

// installScriptURL is the installer for the named platform.
func installScriptURL(goos string) string {
	if goos == "windows" {
		return InstallScriptWindowsURL
	}
	return InstallScriptURL
}

// fetchInstallScript reads the installer. Its own timeout bounds this rather
// than the caller's context, which has to stay open for however long the
// download the script itself runs takes.
func fetchInstallScript(ctx context.Context, opts InstallOptions) ([]byte, error) {
	endpoint := opts.ScriptURL
	if endpoint == "" {
		endpoint = installScriptURL(runtime.GOOS)
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build the installer request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download the installer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the installer download answered %s", resp.Status)
	}

	script, err := io.ReadAll(io.LimitReader(resp.Body, maxScriptBytes))
	if err != nil {
		return nil, fmt.Errorf("read the installer: %w", err)
	}
	if len(script) == 0 {
		return nil, errors.New("the installer download was empty")
	}

	return script, nil
}

// stageScript writes the installer to a temporary file and returns it with the
// removal. The extension is not decoration: PowerShell refuses -File on a path
// that is not .ps1.
func stageScript(goos string, script []byte) (string, func(), error) {
	pattern := "zen-linear-install-*.sh"
	if goos == "windows" {
		pattern = "zen-linear-install-*.ps1"
	}

	file, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("stage the installer: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }

	if _, err := file.Write(script); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("stage the installer: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage the installer: %w", err)
	}

	return path, cleanup, nil
}

// installerArgs is the argv for the staged script. Windows bypasses the
// execution policy for this one invocation, which is what irm | iex amounts to
// and what a script arriving from the network otherwise fails on.
func installerArgs(goos, script string) (string, []string) {
	if goos == "windows" {
		return "powershell", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script}
	}
	return "sh", []string{script}
}

// runInstallScript executes the staged installer, wiring its output straight
// through so the script's own messages are what the user reads.
func runInstallScript(ctx context.Context, script, dir string, out io.Writer) error {
	name, args := installerArgs(runtime.GOOS, script)

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "INSTALL_DIR="+dir)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run the installer: %w", err)
	}

	return nil
}

// InstallDir is the directory the running binary is in, symlinks resolved, so
// an upgrade replaces the copy on the user's PATH rather than leaving it stale
// beside a second one in the installer's default location.
func InstallDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve the running binary: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve the running binary: %w", err)
	}

	return filepath.Dir(resolved), nil
}
