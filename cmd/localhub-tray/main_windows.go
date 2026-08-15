//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"github.com/localhub/localhub/assets"
	"github.com/localhub/localhub/internal/startupconfig"
	"github.com/localhub/localhub/internal/trayapp"
	"golang.org/x/sys/windows"
)

const trayMutexName = `Local\LocalHubCompassTray`

// Every busy action is bounded. These are ceilings, not waits: an action returns as soon as it is
// done. Their real job is to guarantee that the menu comes back even when something below never
// answers, which is the failure that made the tray unusable.
const (
	startupActionTimeout  = 3 * time.Minute
	recoveryActionTimeout = 6 * time.Minute
	shutdownTimeout       = 6 * time.Minute
	statusTimeout         = 5 * time.Second
)

type tray struct {
	controller   *trayapp.Controller
	done         chan struct{}
	closeOnce    sync.Once
	busy         atomic.Bool
	exiting      atomic.Bool
	actionMu     sync.Mutex
	actionCancel context.CancelFunc
	messageMu    sync.Mutex
	message      string
	messageEnd   time.Time
	configMu     sync.Mutex
	config       startupconfig.StartupConfig
	attempts     map[string]int
	modelMenus   map[string]*systray.MenuItem
	delayItems   map[int]*systray.MenuItem

	// nextEnsure is the earliest time the recovery pass may run again, as Unix nanoseconds.
	// It is written by action goroutines and read by the event loop, so it is atomic rather
	// than a plain time.Time.
	nextEnsure atomic.Int64

	statusItem   *systray.MenuItem
	openItem     *systray.MenuItem
	startHubItem *systray.MenuItem
	exitItem     *systray.MenuItem
	startupMenu  *systray.MenuItem
	modelsMenu   *systray.MenuItem
}

func main() {
	var repoRoot string
	var webURL string
	var shutdownOnly bool
	flag.StringVar(&repoRoot, "repo", "", "LocalHub repository/runtime directory")
	flag.StringVar(&webURL, "web-url", trayapp.DefaultWebURL, "LocalHub web UI URL")
	flag.BoolVar(&shutdownOnly, "shutdown", false, "stop LocalHub models and daemon, then exit without opening the tray")
	flag.Parse()

	// Resolve repoRoot and open the log file before anything else can exit the process, so both
	// silent-exit paths below (mutex acquisition, controller construction) always log why.
	if repoRoot == "" {
		repoRoot = findRepoRoot()
	}
	logPath := trayapp.InitLogger(repoRoot)
	trayapp.Logf("tray starting: pid=%d repo=%q web-url=%q log=%q", os.Getpid(), repoRoot, webURL, logPath)
	if shutdownOnly {
		controller, err := trayapp.New(repoRoot, webURL)
		if err != nil {
			trayapp.Logf("shutdown-only controller construction failed: %v", err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := controller.ShutdownAll(ctx); err != nil {
			trayapp.Logf("shutdown-only request failed: %v", err)
			os.Exit(1)
		}
		trayapp.Logf("shutdown-only request completed: all LocalHub processes are down")
		return
	}

	mutex, alreadyRunning, err := acquireSingleInstance()
	if err != nil {
		trayapp.Logf("startup aborted: could not acquire single-instance mutex: %v", err)
		return
	}
	if alreadyRunning {
		trayapp.Logf("startup aborted: another LocalHub tray instance is already running")
		return
	}
	defer windows.CloseHandle(mutex)

	controller, err := trayapp.New(repoRoot, webURL)
	if err != nil {
		trayapp.Logf("startup aborted: controller construction failed: %v", err)
		return
	}
	application := &tray{
		controller: controller,
		done:       make(chan struct{}),
		attempts:   make(map[string]int),
		modelMenus: make(map[string]*systray.MenuItem),
		delayItems: make(map[int]*systray.MenuItem),
	}
	trayapp.Logf("tray initialized, entering systray.Run")
	systray.Run(application.onReady, application.onExit)
	trayapp.Logf("tray shutdown: systray.Run returned")
}

func (t *tray) onReady() {
	systray.SetIcon(localHubIcon())
	systray.SetTooltip("LocalHub Compass")
	systray.SetOnTapped(func() { t.openWebUI() })

	t.statusItem = systray.AddMenuItem("LocalHub: checking…", "Current LocalHub control-plane status")
	t.statusItem.Disable()
	// Open Web UI is never disabled. It only hands a URL to the default browser, so there is no
	// state in which greying it out tells the operator anything useful — and an unexplained grey
	// item is precisely the thing that made this menu feel broken.
	t.openItem = systray.AddMenuItem("Open Web UI", "Open LocalHub Compass in your default browser")
	systray.AddSeparator()
	t.startHubItem = systray.AddMenuItem("Start LocalHub", "Start the LocalHub control plane")
	systray.AddSeparator()
	t.startupMenu = systray.AddMenuItem("Startup behavior", "Customize Windows, LocalHub, and model startup")
	t.modelsMenu = systray.AddMenuItem("Models at startup", "Choose models, launch profiles, and retry policy")
	systray.AddSeparator()
	t.exitItem = systray.AddMenuItem("Exit LocalHub", "Stop running models, then close this tray app")

	t.configureStartupMenus()
	trayapp.SafeGo("eventLoop", t.eventLoop)
	// Paint real status before anything else runs, so the first thing the operator sees is the
	// truth about the daemon rather than a placeholder that may never be replaced.
	trayapp.SafeGo("initialStatus", func() { t.refreshStatus("") })
	trayapp.SafeGo("runConfiguredStartup", t.runConfiguredStartup)
}

func (t *tray) onExit() {
	t.closeOnce.Do(func() { close(t.done) })
}

// ensureDue reports whether the periodic recovery pass is allowed to run again.
func (t *tray) ensureDue() bool {
	return time.Now().UnixNano() >= t.nextEnsure.Load()
}

// scheduleEnsure defers the next recovery pass by the configured health-check interval.
func (t *tray) scheduleEnsure(seconds int) {
	t.nextEnsure.Store(time.Now().Add(time.Duration(seconds) * time.Second).UnixNano())
}

func (t *tray) eventLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			if !t.busy.Load() {
				t.refreshStatus("")
				if t.ensureDue() {
					trayapp.SafeGo("ensureConfiguredModels", t.ensureConfiguredModels)
				}
			}
		case <-t.openItem.ClickedCh:
			t.openWebUI()
		case <-t.startHubItem.ClickedCh:
			trayapp.SafeGo("startLocalHub", func() {
				t.withBusy("Starting LocalHub…", startupActionTimeout, t.controller.StartLocalHub)
			})
		case <-t.exitItem.ClickedCh:
			trayapp.SafeGo("exitAll", t.exitAll)
		}
	}
}

// withBusy runs one tray action under the single-action lock with a bounded deadline.
//
// This is the structural fix for the bug that made the tray useless: the menu was disabled at the
// start of an action and re-enabled at each success path, so any action that failed to return —
// and the PowerShell shell-out reliably failed to return — left every item greyed out forever,
// with "Starting LocalHub..." frozen in the status line.
//
// Here, restoring the menu is a deferred obligation, not a step someone can forget or skip. It
// runs on success, on error, on timeout, and on panic. There is no code path through this function
// that leaves the menu disabled, and there is no way to enter the busy state except through it.
func (t *tray) withBusy(label string, timeout time.Duration, action func(context.Context) error) {
	if t.exiting.Load() {
		return
	}
	if !t.busy.CompareAndSwap(false, true) {
		trayapp.Logf("skipped %q: another tray action is already running", label)
		return
	}
	var actionErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			trayapp.Logf("PANIC in tray action %q: %v\n%s", label, recovered, debug.Stack())
			actionErr = fmt.Errorf("internal error: %v", recovered)
		}
		t.busy.Store(false)
		if actionErr != nil {
			t.showActionError(actionErr)
			return
		}
		t.refreshStatus("")
	}()

	t.setBusy(label)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.actionMu.Lock()
	t.actionCancel = cancel
	t.actionMu.Unlock()
	defer func() {
		cancel()
		t.actionMu.Lock()
		t.actionCancel = nil
		t.actionMu.Unlock()
	}()
	actionErr = action(ctx)
	if errors.Is(actionErr, context.DeadlineExceeded) {
		// Report the deadline in the operator's terms. "context deadline exceeded" in a tray menu
		// says nothing about which action gave up, or after how long.
		actionErr = fmt.Errorf("%s gave up after %s", trimEllipsis(label), timeout)
	}
}

func trimEllipsis(label string) string {
	for _, suffix := range []string{"…", "..."} {
		if len(label) > len(suffix) && label[len(label)-len(suffix):] == suffix {
			return label[:len(label)-len(suffix)]
		}
	}
	return label
}

// exitAll bypasses the ordinary busy gate, cancels any startup/recovery action, and quits only
// after the model processes and verified LocalHub daemon are gone.
func (t *tray) exitAll() {
	if !t.exiting.CompareAndSwap(false, true) {
		return
	}
	t.exitItem.Disable()
	t.statusItem.SetTitle("Stopping running models…")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	t.cancelActiveAction()
	for t.busy.Load() {
		select {
		case <-ctx.Done():
			t.exiting.Store(false)
			t.showActionError(fmt.Errorf("Exit incomplete: another LocalHub action did not stop: %w", ctx.Err()))
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := t.controller.ShutdownAll(ctx); err != nil {
		trayapp.Logf("shutdown did not complete cleanly; keeping tray open: %v", err)
		t.exiting.Store(false)
		t.showActionError(fmt.Errorf("Exit incomplete: %w", err))
		return
	}
	systray.Quit()
}

func (t *tray) cancelActiveAction() {
	t.actionMu.Lock()
	defer t.actionMu.Unlock()
	if t.actionCancel != nil {
		t.actionCancel()
	}
}

func (t *tray) showActionError(err error) {
	message := friendlyError(err)
	t.rememberMessage(message, 45*time.Second)
	t.refreshStatus(message)
}

func (t *tray) openWebUI() {
	if err := t.controller.OpenWebUI(); err != nil {
		message := "Could not open Web UI"
		t.rememberMessage(message, 20*time.Second)
		t.refreshStatus(message)
	}
}

// refreshStatus repaints the status line and re-enables every item the current state allows. It is
// the only place that can leave the menu usable again, and withBusy always defers to it.
func (t *tray) refreshStatus(override string) {
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	status := t.controller.Status(ctx)
	if override == "" {
		override = t.currentMessage()
	}
	if override != "" {
		t.statusItem.SetTitle(override)
		systray.SetTooltip("LocalHub Compass — " + override)
	} else {
		hubState := "stopped"
		if status.LocalHubHealthy {
			hubState = "running"
		}
		t.statusItem.SetTitle(fmt.Sprintf("LocalHub: %s", hubState))
		systray.SetTooltip("LocalHub Compass")
	}

	t.openItem.Enable()
	if status.LocalHubHealthy {
		// Disabled here means one specific, self-explanatory thing, and the title says it outright
		// rather than leaving the operator to guess why the item is grey.
		t.startHubItem.SetTitle("LocalHub is running")
		t.startHubItem.Disable()
	} else {
		t.startHubItem.SetTitle("Start LocalHub")
		t.startHubItem.Enable()
	}
	if t.exiting.Load() {
		t.exitItem.Disable()
	} else {
		t.exitItem.Enable()
	}
}

// setBusy marks one action in flight. It disables only what the action itself would conflict with:
// Open Web UI and Exit stay live, so a slow or failing action can neither hide the dashboard nor
// trap the operator in a tray app they cannot close.
func (t *tray) setBusy(label string) {
	t.statusItem.SetTitle(label)
	t.startHubItem.Disable()
	t.openItem.Enable()
	t.exitItem.Enable()
}

func (t *tray) rememberMessage(message string, duration time.Duration) {
	// Every user-facing failure (daemon unreachable, action failed, shutdown incomplete, ...)
	// flows through here, so this is the one place that needs to log it for observability.
	trayapp.Logf("tray message: %s", message)
	t.messageMu.Lock()
	defer t.messageMu.Unlock()
	t.message = message
	t.messageEnd = time.Now().Add(duration)
}

func (t *tray) currentMessage() string {
	t.messageMu.Lock()
	defer t.messageMu.Unlock()
	if time.Now().After(t.messageEnd) {
		t.message = ""
		return ""
	}
	return t.message
}

func shortError(err error) string {
	runes := []rune(err.Error())
	if len(runes) > 80 {
		return string(runes[:77]) + "…"
	}
	return string(runes)
}

var vramRefusalPattern = regexp.MustCompile(`Refusing fresh load: (\d+) MiB VRAM is free; this profile requires at least (\d+) MiB`)

// friendlyError turns the daemon's admission refusal into the one sentence that actually helps.
// Everything else keeps its reason text, per the project rule that every user-facing failure
// carries a human-readable reason.
func friendlyError(err error) string {
	if freeMiB, requiredMiB, ok := vramShortage(err); ok {
		return fmt.Sprintf("Not enough VRAM: %s MiB free; %s MiB required", formatNumber(freeMiB), formatNumber(requiredMiB))
	}
	return "Action failed: " + shortError(err)
}

func vramShortage(err error) (freeMiB, requiredMiB int, ok bool) {
	match := vramRefusalPattern.FindStringSubmatch(err.Error())
	if len(match) != 3 {
		return 0, 0, false
	}
	freeMiB, freeErr := strconv.Atoi(match[1])
	requiredMiB, requiredErr := strconv.Atoi(match[2])
	return freeMiB, requiredMiB, freeErr == nil && requiredErr == nil
}

func formatNumber(value int) string {
	if value < 1000 {
		return strconv.Itoa(value)
	}
	return fmt.Sprintf("%d,%03d", value/1000, value%1000)
}

// findRepoRoot locates the LocalHub runtime directory when -repo was not supplied.
//
// It walks up from the executable rather than testing one fixed path, so the tray works whether it
// was built to the repo root (`go build -o localhub-tray.exe ./cmd/localhub-tray`) or into bin/.
// A directory is the runtime root if it holds the config the daemon is launched with.
func findRepoRoot() string {
	if executable, err := os.Executable(); err == nil {
		directory := filepath.Dir(executable)
		for depth := 0; depth < 4; depth++ {
			if isRepoRoot(directory) {
				return directory
			}
			parent := filepath.Dir(directory)
			if parent == directory {
				break
			}
			directory = parent
		}
	}
	if current, err := os.Getwd(); err == nil {
		return current
	}
	return "."
}

func isRepoRoot(directory string) bool {
	return fileExists(filepath.Join(directory, "configs", "config.yaml")) ||
		fileExists(filepath.Join(directory, "localhub.exe")) ||
		fileExists(filepath.Join(directory, "bin", "localhub.exe"))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func acquireSingleInstance() (windows.Handle, bool, error) {
	name, err := windows.UTF16PtrFromString(trayMutexName)
	if err != nil {
		return 0, false, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return 0, true, nil
	}
	return handle, false, err
}

// localHubIcon returns the shared multi-resolution icon.
//
// It replaces a 16x16 bitmap this file used to build byte by byte at runtime. That icon was the
// only one LocalHub had, so Start Menu shortcuts had nothing to point at and fell back to the
// generic executable glyph. The same assets/localhub.ico is now embedded here and referenced by
// scripts/install-start-menu.ps1, so the tray and the shortcut show one mark.
func localHubIcon() []byte {
	return assets.Icon
}
