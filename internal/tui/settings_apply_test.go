package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/praxis-labs-io/zen-linear/internal/config"
	"github.com/praxis-labs-io/zen-linear/internal/linearapi"
	"github.com/praxis-labs-io/zen-linear/internal/logger"
)

// TestApplySettingsPreservesOAuthBearer guards the bug where an in-app settings
// save rebuilt the API client from config alone, dropping the OAuth bearer
// scheme and downgrading the session to raw-token auth Linear then rejects.
func TestApplySettingsPreservesOAuthBearer(t *testing.T) {
	var mu sync.Mutex
	var projectsAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		query := strings.ToLower(request.Query)

		var data any
		switch {
		case strings.Contains(query, "viewer"):
			data = map[string]any{"viewer": map[string]any{
				"id": "user-1", "name": "Test User", "displayName": "Test User", "email": "test@example.com",
			}}
		case strings.Contains(query, "teams"):
			data = map[string]any{"teams": map[string]any{"nodes": []any{
				map[string]any{"id": "team-2", "key": "NEX", "name": "Nexa"},
			}}}
		case strings.Contains(query, "favorites"):
			data = map[string]any{"favorites": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			}}
		case strings.Contains(query, "projects"):
			mu.Lock()
			projectsAuth = auth
			mu.Unlock()
			data = map[string]any{"team": map[string]any{"projects": map[string]any{"nodes": []any{
				map[string]any{"id": "proj-1", "name": "Website"},
			}}}}
		case strings.Contains(query, "states"):
			data = map[string]any{"team": map[string]any{"states": map[string]any{"nodes": []any{}}}}
		case strings.Contains(query, "cycles"):
			data = map[string]any{"team": map[string]any{"cycles": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			}}}
		case strings.Contains(query, "issues"):
			data = map[string]any{"issues": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			}}
		default:
			data = map[string]any{}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Errorf("encode GraphQL response: %v", err)
		}
	}))
	defer server.Close()

	cfg := config.Config{
		APIEndpoint:  server.URL,
		LinearAPIKey: "oauth-token",
		CacheTTL:     time.Minute,
		PageSize:     10,
	}
	app := NewApp(linearapi.ClientConfig{
		Token:     "oauth-token",
		Endpoint:  server.URL,
		UseBearer: true,
	}, cfg, nil)
	startReviewTestApplication(t, app)
	refreshDone := installRefreshCompletionHook(app)

	// The timeout is one of the three fields that cost a new client, so this is
	// a save that actually rebuilds one. Applying an unchanged config would
	// leave the launch client in place and assert nothing.
	saved := cfg
	saved.Timeout = 45 * time.Second
	app.applySettings(saved)

	if _, err := app.fetchProjectsFunc(context.Background(), "team-2"); err != nil {
		t.Fatalf("fetchProjectsFunc() error: %v", err)
	}

	mu.Lock()
	got := projectsAuth
	mu.Unlock()
	if got != "Bearer oauth-token" {
		t.Fatalf("Authorization after settings save = %q, want %q", got, "Bearer oauth-token")
	}
	waitForRefreshCompletion(t, refreshDone)
}

// isolateLogging keeps a test that reinitializes the process-global logger from
// writing into the developer's own ~/.zen-linear and from leaving the logger
// pointing at a temp directory the next test no longer has.
func isolateLogging(t *testing.T) {
	t.Helper()
	setHomeDir(t, t.TempDir())
	t.Cleanup(func() {
		if _, warning := logger.Restart("", "", logger.LevelWarning); warning != "" {
			t.Errorf("restoring the logger: %s", warning)
		}
	})
}

// A log path the app cannot open used to abort applySettings: logger.Reinit
// returned an error, the handler reported it and returned early, and everything
// after it — the rebuilt API client included — never ran. Saving a bad log path
// took the rest of the settings with it.
func TestApplySettingsSurvivesAnUnwritableLogPath(t *testing.T) {
	isolateLogging(t)

	app := newUXTestApp(t)

	tmpDir := t.TempDir()
	// A regular file where the refused path wants a directory.
	blocker := filepath.Join(tmpDir, "blocker")
	if err := os.WriteFile(blocker, nil, 0644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	refused := filepath.Join(blocker, "nested", "app.log")

	// The API client is rebuilt after the logger line, so the old pointer
	// surviving is what an early return there looks like from outside.
	before := app.api

	cfg := app.config
	cfg.APIEndpoint = "https://example.invalid/graphql"
	cfg.LogFile = refused
	app.applySettings(cfg)

	if app.api == before {
		t.Error("API client not rebuilt: applySettings stopped at the refused log path")
	}

	// The config names where logs actually go, not the path that was refused.
	if app.config.LogFile == refused {
		t.Errorf("config.LogFile = %q, want the path actually opened", app.config.LogFile)
	}
	// Held for the reload to settle, since the reload repaints the hint line.
	if !strings.Contains(app.pendingWarning, refused) {
		t.Errorf("held warning %q does not name the refused path %q", app.pendingWarning, refused)
	}

	// And it lands on the hint line rather than the toast corner, which
	// truncates to half the row and would drop the half that says what happened.
	app.reportPendingWarning()
	if status := app.statusBar.GetText(true); !strings.Contains(status, refused) {
		t.Errorf("status %q does not name the refused path %q", status, refused)
	}
	if app.statusMessage != "" {
		t.Errorf("toast corner = %q, want the warning on the hint line", app.statusMessage)
	}
}

// Falling back all the way to no logging is where this save landed, not a
// setting the user chose. Adopting it would write "log_file": "" on the next
// save and turn one unwritable path into logging off for good.
func TestApplySettingsDoesNotAdoptLoggingOffAsASetting(t *testing.T) {
	isolateLogging(t)

	app := newUXTestApp(t)

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	refused := filepath.Join(blocker, "nested", "app.log")

	// Nowhere left to fall back to: HOME is a file, so the default fails too.
	home := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(home, nil, 0644); err != nil {
		t.Fatalf("write home: %v", err)
	}
	setHomeDir(t, home)

	cfg := app.config
	cfg.LogFile = refused
	app.applySettings(cfg)

	if app.config.LogFile == "" {
		t.Error("config.LogFile = \"\", which saves as a deliberate logging-off")
	}
	if got := config.SettingsFromConfig(app.config).LogFile; got != nil && *got == "" {
		t.Error("settings would write log_file: \"\" after a failed open")
	}
}

// seedPlace fills the state a settings save used to throw away: the list the
// user is on, the rows in front of them, their filters and the team metadata
// already fetched for the choosers.
func seedPlace(app *App) {
	app.selectedNavigation = &NavigationNode{ID: "team-1", TeamID: "team-1", IsTeam: true, Text: "Engineering"}
	app.issues = []linearapi.Issue{{ID: "issue-1", Identifier: "ZNL-1", Title: "On screen"}}
	app.listIssueRows = []IssueRow{{IssueID: "issue-1"}}
	app.richFilters = IssueFilters{AssigneeID: "user-1", AssigneeName: "Test User"}
	app.collapsedGroups = map[string]bool{"Todo": true}
	app.metadataTeamID = "team-1"
	app.teamProjects = []linearapi.Project{{ID: "proj-1", Name: "Website"}}
	app.groupingOverridden = true
}

// A save that changes nothing about the connection has no reason to throw away
// what came through it. Before this, saving a theme dropped the list, the
// selection, the filters and every cached option, and pulled the workspace
// again over the network.
func TestSavingAThemeKeepsWhatIsOnScreen(t *testing.T) {
	app := newUXTestApp(t)
	seedPlace(app)

	before := app.api
	generation := app.resetGeneration.Load()

	cfg := app.config
	cfg.Theme = config.ThemeLinear
	app.applySettings(cfg)

	// The save did land, so the rest is not a no-op mistaken for a pass.
	if app.theme != ResolveTheme(config.ThemeLinear) {
		t.Fatal("theme not applied: the save did not take")
	}

	if app.api != before {
		t.Error("API client rebuilt for a theme change")
	}
	if got := app.resetGeneration.Load(); got != generation {
		t.Errorf("resetGeneration = %d, want %d: the cached state was reset", got, generation)
	}
	if app.selectedNavigation == nil {
		t.Error("navigation selection dropped")
	}
	if len(app.listIssueRows) == 0 {
		t.Error("issue rows dropped")
	}
	if app.richFilters.AssigneeID != "user-1" {
		t.Errorf("richFilters.AssigneeID = %q, want the filter the user set", app.richFilters.AssigneeID)
	}
	if !app.collapsedGroups["Todo"] {
		t.Error("collapsed groups dropped")
	}
	if app.metadataTeamID != "team-1" || len(app.teamProjects) == 0 {
		t.Error("team metadata dropped, costing a refetch the save did not need")
	}
	if !app.groupingOverridden {
		t.Error("grouping override dropped, so a custom view's preference would outrank the user's choice")
	}
}

// Rebuilding the modals shells out to the agent CLI for its model list, so an
// unconditional rebuild put a subprocess behind every save. The modal pointers
// are the cheapest thing that says whether it ran.
func TestSavingAPageSizeRebuildsNoModals(t *testing.T) {
	app := newUXTestApp(t)

	before := app.settingsModal

	cfg := app.config
	cfg.PageSize = app.config.PageSize + 10
	app.applySettings(cfg)

	if app.settingsModal != before {
		t.Error("modals rebuilt for a page size change")
	}
	if app.config.PageSize != cfg.PageSize {
		t.Errorf("config.PageSize = %d, want %d", app.config.PageSize, cfg.PageSize)
	}
}

// Reinit writes a session marker and reopens the file, so an unconditional
// restart planted a false "the app just started" line in the user's log on
// every save.
func TestSavingAThemeDoesNotRestartLogging(t *testing.T) {
	isolateLogging(t)

	app := newUXTestApp(t)

	logPath := filepath.Join(t.TempDir(), "app.log")
	cfg := app.config
	cfg.LogFile = logPath
	cfg.LogLevel = "debug"
	app.applySettings(cfg)

	themed := app.config
	themed.Theme = config.ThemeLinear
	app.applySettings(themed)

	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if got := strings.Count(string(contents), "=== Session started ==="); got != 1 {
		t.Errorf("session markers = %d, want 1: the theme save reopened the log", got)
	}
}

// The other side of the tier: the endpoint is one of the three settings whose
// change invalidates everything already fetched.
func TestSavingANewEndpointReloads(t *testing.T) {
	app := newUXTestApp(t)
	seedPlace(app)

	before := app.api
	generation := app.resetGeneration.Load()

	cfg := app.config
	cfg.APIEndpoint = "https://example.invalid/graphql"
	app.applySettings(cfg)

	if app.api == before {
		t.Error("API client not rebuilt for a new endpoint")
	}
	if got := app.resetGeneration.Load(); got == generation {
		t.Error("cached state not reset for a new endpoint")
	}
	if len(app.listIssueRows) != 0 {
		t.Error("issue rows survived a connection change, so the pane shows another workspace's issues")
	}
}

// A connection change is the one save that still reloads, and the reload used
// to reopen on default_team / All Issues. The place is the user's, not the
// connection's: it goes to the one-shot restore loadInitialData runs, so a save
// made for one setting does not also move them.
func TestSavingANewConnectionPutsTheUserBackWhereTheyWere(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		query := strings.ToLower(request.Query)

		var data any
		switch {
		case strings.Contains(query, "viewer"):
			data = map[string]any{"viewer": map[string]any{
				"id": "user-1", "name": "Test User", "displayName": "Test User", "email": "test@example.com",
			}}
		case strings.Contains(query, "teams"):
			data = map[string]any{"teams": map[string]any{"nodes": []any{
				map[string]any{"id": "team-1", "key": "ENG", "name": "Engineering"},
				map[string]any{"id": "team-2", "key": "NEX", "name": "Nexa"},
			}}}
		case strings.Contains(query, "favorites"):
			data = map[string]any{"favorites": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			}}
		case strings.Contains(query, "issues"):
			data = map[string]any{"issues": map[string]any{
				"nodes":    []any{},
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			}}
		default:
			data = map[string]any{}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Errorf("encode GraphQL response: %v", err)
		}
	}))
	defer server.Close()

	cfg := config.Config{
		APIEndpoint:  server.URL,
		LinearAPIKey: "token",
		CacheTTL:     time.Minute,
		PageSize:     10,
		// The counterfactual: without the re-seed the reload falls through to
		// this, and the user ends up on Nexa for having changed a timeout.
		DefaultTeam: "NEX",
	}
	app := NewApp(linearapi.ClientConfig{Token: "token", Endpoint: server.URL}, cfg, nil)
	startReviewTestApplication(t, app)
	navDone := installNavSettledHook(app)
	refreshDone := installRefreshCompletionHook(app)

	saved := cfg
	saved.Timeout = 45 * time.Second
	// On the event loop, where the Save button runs it. Off it, the restore
	// this ends in repaints the status bar under the draw goroutine.
	app.app.QueueUpdate(func() {
		// Where the user is when they open settings.
		app.selectedNavigation = &NavigationNode{ID: "team-1", TeamID: "team-1", IsTeam: true, Text: "Engineering"}
		app.applySettings(saved)
	})

	waitForNavSettled(t, navDone)
	waitForRefreshCompletion(t, refreshDone)

	var selected *NavigationNode
	app.app.QueueUpdate(func() { selected = app.selectedNavigation })
	if selected == nil {
		t.Fatal("navigation selection empty after the reload")
	}
	if got := selected.TeamID; got != "team-1" {
		t.Errorf("selected team = %q, want %q: the reload moved the user to the configured default", got, "team-1")
	}
}
