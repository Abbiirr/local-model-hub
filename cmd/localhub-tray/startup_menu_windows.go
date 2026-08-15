//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fyne.io/systray"
	"github.com/localhub/localhub/internal/startupconfig"
	"github.com/localhub/localhub/internal/trayapp"
)

func (t *tray) configureStartupMenus() {
	cfg, err := t.controller.LoadStartupConfig()
	if err != nil {
		cfg = startupconfig.DefaultStartupConfig()
		t.rememberMessage("Startup settings failed: "+shortError(err), 45*time.Second)
	}
	t.configMu.Lock()
	t.config = cfg
	t.configMu.Unlock()
	t.scheduleEnsure(cfg.HealthCheckSeconds)

	startWindows := t.startupMenu.AddSubMenuItemCheckbox(
		"Start with Windows", "Also controllable from Task Manager Startup apps", t.controller.StartupEnabled())
	startHub := t.startupMenu.AddSubMenuItemCheckbox(
		"Start LocalHub", "Start the control plane when this app starts", cfg.StartLocalHub)
	openUI := t.startupMenu.AddSubMenuItemCheckbox(
		"Open Web UI", "Open the dashboard after startup", cfg.OpenWebUI)
	restore := t.startupMenu.AddSubMenuItemCheckbox(
		"Restore last session", "Restore models that were running previously", cfg.RestoreLastSession)
	delayMenu := t.startupMenu.AddSubMenuItem("Startup delay", "Delay startup after Windows login")

	trayapp.SafeGo("toggleWindowsStartup", func() { t.toggleWindowsStartup(startWindows) })
	trayapp.SafeGo("toggleConfigBool:StartLocalHub", func() {
		t.toggleConfigBool(startHub, func(c *startupconfig.StartupConfig, value bool) { c.StartLocalHub = value })
	})
	trayapp.SafeGo("toggleConfigBool:OpenWebUI", func() {
		t.toggleConfigBool(openUI, func(c *startupconfig.StartupConfig, value bool) { c.OpenWebUI = value })
	})
	trayapp.SafeGo("toggleConfigBool:RestoreLastSession", func() {
		t.toggleConfigBool(restore, func(c *startupconfig.StartupConfig, value bool) { c.RestoreLastSession = value })
	})
	for _, seconds := range []int{0, 15, 30, 60, 120} {
		label := "No delay"
		if seconds > 0 {
			label = fmt.Sprintf("%d seconds", seconds)
		}
		item := delayMenu.AddSubMenuItemCheckbox(
			label, "Delay before LocalHub and models start", cfg.StartupDelaySeconds == seconds)
		t.delayItems[seconds] = item
		trayapp.SafeGo("chooseStartupDelay", func() { t.chooseStartupDelay(item, seconds) })
	}
	// Build configured model entries while onReady is still constructing the native menu. Adding
	// the first submenu later from the startup worker can leave the Windows menu mutation blocked,
	// which in turn leaves busy=true and removes the submenu arrow. Configured entries are available
	// before LocalHub is healthy, so they form a reliable immediate menu; the later inventory pass
	// only supplements models that are not already configured.
	for _, model := range cfg.Models {
		t.addModelMenu(trayModelFromConfig(model))
	}
}

func (t *tray) toggleWindowsStartup(item *systray.MenuItem) {
	for {
		select {
		case <-t.done:
			return
		case <-item.ClickedCh:
			enabled := !t.controller.StartupEnabled()
			if err := t.controller.SetStartupEnabled(enabled); err != nil {
				t.showActionError(err)
				continue
			}
			setChecked(item, enabled)
		}
	}
}

func (t *tray) toggleConfigBool(item *systray.MenuItem, update func(*startupconfig.StartupConfig, bool)) {
	for {
		select {
		case <-t.done:
			return
		case <-item.ClickedCh:
			value := !item.Checked()
			if err := t.updateConfig(func(cfg *startupconfig.StartupConfig) { update(cfg, value) }); err != nil {
				t.showActionError(err)
				continue
			}
			setChecked(item, value)
		}
	}
}

func (t *tray) chooseStartupDelay(item *systray.MenuItem, seconds int) {
	for {
		select {
		case <-t.done:
			return
		case <-item.ClickedCh:
			if err := t.updateConfig(func(cfg *startupconfig.StartupConfig) {
				cfg.StartupDelaySeconds = seconds
			}); err != nil {
				t.showActionError(err)
				continue
			}
			for value, candidate := range t.delayItems {
				setChecked(candidate, value == seconds)
			}
		}
	}
}

func setChecked(item *systray.MenuItem, checked bool) {
	if checked {
		item.Check()
	} else {
		item.Uncheck()
	}
}

func (t *tray) updateConfig(update func(*startupconfig.StartupConfig)) error {
	t.configMu.Lock()
	defer t.configMu.Unlock()
	copy := t.config
	copy.Models = append([]startupconfig.StartupModelConfig(nil), t.config.Models...)
	update(&copy)
	if err := t.controller.SaveStartupConfig(copy); err != nil {
		return err
	}
	// SaveStartupConfig normalizes a copy, so normalize this in-memory copy too.
	if err := copy.ValidateAndNormalize(); err != nil {
		return err
	}
	t.config = copy
	return nil
}

func (t *tray) currentConfig() startupconfig.StartupConfig {
	t.configMu.Lock()
	defer t.configMu.Unlock()
	copy := t.config
	copy.Models = append([]startupconfig.StartupModelConfig(nil), t.config.Models...)
	return copy
}

// runConfiguredStartup applies the saved startup intent once, at launch.
//
// The login delay happens outside the busy gate: waiting is not an action, and disabling the menu
// for up to two minutes because the operator configured a delay is indistinguishable from the tray
// being broken. Everything after it runs inside withBusy, which guarantees the menu comes back.
func (t *tray) runConfiguredStartup() {
	cfg := t.currentConfig()
	if cfg.StartupDelaySeconds > 0 {
		t.statusItem.SetTitle(fmt.Sprintf("Startup delayed %d seconds", cfg.StartupDelaySeconds))
		select {
		case <-t.done:
			return
		case <-time.After(time.Duration(cfg.StartupDelaySeconds) * time.Second):
		}
	}
	t.withBusy("Starting LocalHub…", startupActionTimeout, func(ctx context.Context) error {
		if cfg.StartLocalHub {
			// StartLocalHub health-checks before it starts anything, so an already-running daemon
			// is adopted rather than fought over.
			if err := t.controller.StartLocalHub(ctx); err != nil {
				return err
			}
		}
		if cfg.OpenWebUI {
			_ = t.controller.OpenWebUI()
		}
		t.buildModelMenus(ctx)
		cfg = t.controller.RestoreLastSession(cfg)
		results := t.controller.RunStartup(ctx, cfg)
		for _, result := range results {
			t.attempts[result.ModelID]++
			if !result.Started {
				t.rememberMessage("Startup failed: "+shortError(errors.New(result.Reason)), 45*time.Second)
			}
		}
		t.scheduleEnsure(cfg.HealthCheckSeconds)
		return nil
	})
}

func (t *tray) buildModelMenus(ctx context.Context) {
	models, err := t.controller.Models(ctx)
	if err != nil {
		t.rememberMessage("Could not load model startup menu: "+shortError(err), 45*time.Second)
		return
	}
	for _, model := range models {
		t.addModelMenu(model)
	}
}

func trayModelFromConfig(model startupconfig.StartupModelConfig) trayapp.AvailableModel {
	return trayapp.AvailableModel{ID: model.ModelID, DisplayName: model.ModelID, Presence: "present"}
}

func (t *tray) addModelMenu(model trayapp.AvailableModel) {
	if model.ID == "" {
		return
	}
	if _, exists := t.modelMenus[model.ID]; exists {
		return
	}
	label := model.DisplayName
	if label == "" {
		label = model.ID
	}
	parent := t.modelsMenu.AddSubMenuItem(label, model.LocalPath)
	t.modelMenus[model.ID] = parent
	configured, found := t.findModelConfig(model.ID)
	load := parent.AddSubMenuItemCheckbox(
		"Load at startup", "Load this model after LocalHub starts", found && configured.Enabled)
	profile := parent.AddSubMenuItemCheckbox(
		"Use saved launch profile", "Use the profile saved by LocalHub Advanced start", found && configured.UseSavedProfile)
	retryMenu := parent.AddSubMenuItem("Retry policy", "Choose what happens after a failed load or crash")
	for _, policy := range []string{startupconfig.RetryNever, startupconfig.RetryLimited, startupconfig.RetryKeepAvailable} {
		label := map[string]string{
			startupconfig.RetryNever:         "Do not retry",
			startupconfig.RetryLimited:       "Retry 3 times",
			startupconfig.RetryKeepAvailable: "Keep available",
		}[policy]
		item := retryMenu.AddSubMenuItemCheckbox(label, "Per-model startup recovery policy", found && configured.RetryPolicy == policy)
		trayapp.SafeGo("chooseModelRetry", func() { t.chooseModelRetry(item, model.ID, policy) })
	}
	delayMenu := parent.AddSubMenuItem("Load delay", "Delay this model after the previous model")
	for _, seconds := range []int{0, 15, 30, 60} {
		label := fmt.Sprintf("%d seconds", seconds)
		item := delayMenu.AddSubMenuItemCheckbox(label, "Per-model sequential load delay", found && configured.StartDelaySeconds == seconds)
		trayapp.SafeGo("chooseModelDelay", func() { t.chooseModelDelay(item, model.ID, seconds) })
	}
	earlier := parent.AddSubMenuItem("Move earlier", "Move this model earlier in startup order")
	later := parent.AddSubMenuItem("Move later", "Move this model later in startup order")
	trayapp.SafeGo("toggleStartupModel:enabled", func() { t.toggleStartupModel(load, model.ID, "enabled") })
	trayapp.SafeGo("toggleStartupModel:profile", func() { t.toggleStartupModel(profile, model.ID, "profile") })
	trayapp.SafeGo("moveModel:earlier", func() { t.moveModel(earlier, model.ID, -1) })
	trayapp.SafeGo("moveModel:later", func() { t.moveModel(later, model.ID, 1) })
}

func (t *tray) findModelConfig(modelID string) (startupconfig.StartupModelConfig, bool) {
	cfg := t.currentConfig()
	for _, model := range cfg.Models {
		if model.ModelID == modelID {
			return model, true
		}
	}
	return startupconfig.StartupModelConfig{}, false
}

func (t *tray) mutateModel(cfg *startupconfig.StartupConfig, modelID string, mutate func(*startupconfig.StartupModelConfig)) {
	index := -1
	for i := range cfg.Models {
		if cfg.Models[i].ModelID == modelID {
			index = i
			break
		}
	}
	if index < 0 {
		cfg.Models = append(cfg.Models, startupconfig.StartupModelConfig{
			ModelID: modelID, Order: len(cfg.Models),
			Mode:        startupconfig.StartupModeLocalHubAPI,
			RetryPolicy: startupconfig.RetryNever,
		})
		index = len(cfg.Models) - 1
	}
	mutate(&cfg.Models[index])
}

func (t *tray) toggleStartupModel(item *systray.MenuItem, modelID, field string) {
	for {
		select {
		case <-t.done:
			return
		case <-item.ClickedCh:
			value := !item.Checked()
			err := t.updateConfig(func(cfg *startupconfig.StartupConfig) {
				t.mutateModel(cfg, modelID, func(model *startupconfig.StartupModelConfig) {
					switch field {
					case "enabled":
						model.Enabled = value
					case "profile":
						model.UseSavedProfile = value
					}
				})
			})
			if err != nil {
				t.showActionError(err)
				continue
			}
			setChecked(item, value)
		}
	}
}

func (t *tray) chooseModelRetry(item *systray.MenuItem, modelID, policy string) {
	for {
		select {
		case <-t.done:
			return
		case <-item.ClickedCh:
			err := t.updateConfig(func(cfg *startupconfig.StartupConfig) {
				t.mutateModel(cfg, modelID, func(model *startupconfig.StartupModelConfig) {
					model.RetryPolicy = policy
					model.MaxAttempts = 0
					if policy == startupconfig.RetryLimited {
						model.MaxAttempts = 3
					}
				})
			})
			if err != nil {
				t.showActionError(err)
				continue
			}
			setChecked(item, true)
		}
	}
}

func (t *tray) chooseModelDelay(item *systray.MenuItem, modelID string, seconds int) {
	for {
		select {
		case <-t.done:
			return
		case <-item.ClickedCh:
			err := t.updateConfig(func(cfg *startupconfig.StartupConfig) {
				t.mutateModel(cfg, modelID, func(model *startupconfig.StartupModelConfig) {
					model.StartDelaySeconds = seconds
				})
			})
			if err != nil {
				t.showActionError(err)
				continue
			}
			setChecked(item, true)
		}
	}
}

func (t *tray) moveModel(item *systray.MenuItem, modelID string, delta int) {
	for {
		select {
		case <-t.done:
			return
		case <-item.ClickedCh:
			err := t.updateConfig(func(cfg *startupconfig.StartupConfig) {
				index := -1
				for i := range cfg.Models {
					if cfg.Models[i].ModelID == modelID {
						index = i
						break
					}
				}
				other := index + delta
				if index >= 0 && other >= 0 && other < len(cfg.Models) {
					cfg.Models[index].Order, cfg.Models[other].Order = cfg.Models[other].Order, cfg.Models[index].Order
				}
			})
			if err != nil {
				t.showActionError(err)
			}
		}
	}
}

// ensureConfiguredModels is the periodic recovery pass. It shares the busy gate with every other
// action, so it is bounded and always restores the menu.
func (t *tray) ensureConfiguredModels() {
	t.withBusy("Checking LocalHub…", recoveryActionTimeout, func(ctx context.Context) error {
		cfg, err := t.controller.LoadStartupConfig()
		if err != nil {
			return err
		}
		t.configMu.Lock()
		t.config = cfg
		t.configMu.Unlock()
		// Schedule the next pass before doing any work, so a failure below cannot stop the tray
		// from ever checking again.
		t.scheduleEnsure(cfg.HealthCheckSeconds)

		if cfg.StartLocalHub {
			if err := t.controller.StartLocalHub(ctx); err != nil {
				return err
			}
		}
		for _, model := range cfg.Models {
			if !model.Enabled {
				continue
			}
			if model.RetryPolicy == startupconfig.RetryNever && t.attempts[model.ModelID] > 0 {
				continue
			}
			if model.RetryPolicy == startupconfig.RetryLimited && t.attempts[model.ModelID] >= model.MaxAttempts {
				continue
			}
			t.attempts[model.ModelID]++
			if err := t.controller.StartConfiguredModel(ctx, model); err != nil {
				t.rememberMessage("Recovery waiting: "+shortError(err), 45*time.Second)
			}
		}
		_ = t.controller.SaveLastSession(ctx)
		return nil
	})
}
