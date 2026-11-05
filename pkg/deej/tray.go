package deej

import (
	"fmt"
	"strings"
	"time"

	"fyne.io/systray"

	"github.com/Random-Cow/deej-esphome/pkg/deej/icon"
	"github.com/Random-Cow/deej-esphome/pkg/deej/util"
)

const trayStatusRefreshInterval = 3 * time.Second

// loadTrayIcon returns the embedded PNG icon bytes.
func loadTrayIcon() []byte {
	return icon.DeejLogoPNG
}

func (d *Deej) initializeTray(onDone func()) {
	logger := d.logger.Named("tray")

	onReady := func() {
		logger.Debug("Tray instance ready")

		iconData := loadTrayIcon()
		if len(iconData) == 0 {
			logger.Error("No valid icon data available; tray icon may not display")
		} else {
			systray.SetIcon(iconData)
		}
		systray.SetTitle("deej")
		systray.SetTooltip("deej — volume mixer")

		// ── Status section (disabled items used as read-only labels) ──────────
		connStatusItem := systray.AddMenuItem("ESPHome: connecting…", "ESPHome SSE connection status")
		connStatusItem.Disable()

		sliderStatusItem := systray.AddMenuItem("Sliders: –", "Current slider positions")
		sliderStatusItem.Disable()

		sessionStatusItem := systray.AddMenuItem("Sessions: –", "Number of tracked audio sessions")
		sessionStatusItem.Disable()

		systray.AddSeparator()

		// ── Actions ───────────────────────────────────────────────────────────
		editConfig := systray.AddMenuItem("Edit configuration", "Open config file")
		refreshSessions := systray.AddMenuItem("Re-scan audio sessions", "Manually refresh audio sessions")

		if d.version != "" {
			systray.AddSeparator()
			versionInfo := systray.AddMenuItem(d.version, "")
			versionInfo.Disable()
		}

		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop deej and quit")

		// ── Periodic status updater ───────────────────────────────────────────
		go func() {
			ticker := time.NewTicker(trayStatusRefreshInterval)
			defer ticker.Stop()

			for range ticker.C {
				updateTrayStatus(d, connStatusItem, sliderStatusItem, sessionStatusItem)
			}
		}()

		// ── Event loop ────────────────────────────────────────────────────────
		go func() {
			for {
				select {
				case <-quit.ClickedCh:
					logger.Info("Quit menu item clicked, stopping")
					d.signalStop()

				case <-editConfig.ClickedCh:
					logger.Info("Edit config menu item clicked, opening config for editing")
					if err := util.OpenExternal(logger, "xdg-open", userConfigFilepath); err != nil {
						logger.Warnw("Failed to open config file for editing", "error", err)
					}

				case <-refreshSessions.ClickedCh:
					logger.Info("Refresh sessions menu item clicked, triggering session map refresh")
					d.sessions.refreshSessions(true)
					updateTrayStatus(d, connStatusItem, sliderStatusItem, sessionStatusItem)
				}
			}
		}()

		// actually start the main runtime. this must not block onReady:
		// systray (>= 1.12) only serves the menu over D-Bus after onReady
		// has returned, so a blocking call here leaves the tray menu dead.
		go onDone()
	}

	onExit := func() {
		logger.Debug("Tray exited")
	}

	logger.Debug("Running in tray")
	systray.Run(onReady, onExit)
}

// updateTrayStatus refreshes the disabled status menu items with current data.
func updateTrayStatus(d *Deej, connItem, sliderItem, sessionItem *systray.MenuItem) {
	status := d.GetESPHomeStatus()

	// Connection status line
	if status.Connected {
		uptime := time.Since(status.ConnectedSince).Round(time.Second)
		connLine := fmt.Sprintf("ESPHome %s ✓ (up %s)", status.Address, uptime)
		if status.ReconnectCount > 0 {
			connLine += fmt.Sprintf(", %d reconnect(s)", status.ReconnectCount)
		}
		setStatusTitle(connItem, connLine)
	} else {
		if status.ReconnectCount > 0 {
			setStatusTitle(connItem, fmt.Sprintf("ESPHome %s ✗ (reconnecting, attempt %d)", status.Address, status.ReconnectCount+1))
		} else {
			setStatusTitle(connItem, fmt.Sprintf("ESPHome %s ✗ (connecting…)", status.Address))
		}
	}

	// Slider values line
	sliderNames := status.SliderNames
	if len(status.SliderValues) > 0 {
		parts := make([]string, 0, len(status.SliderValues))
		for i, v := range status.SliderValues {
			label := fmt.Sprintf("%d", i)
			if i < len(sliderNames) {
				label = sliderNames[i]
			}
			if v < 0 {
				parts = append(parts, fmt.Sprintf("%s:–", label))
			} else {
				parts = append(parts, fmt.Sprintf("%s:%d%%", label, int(v*100)))
			}
		}
		setStatusTitle(sliderItem, "Sliders: "+strings.Join(parts, "  "))
	} else {
		setStatusTitle(sliderItem, "Sliders: –")
	}

	// Session count line
	if d.sessions != nil {
		count := d.sessions.SessionCount()
		setStatusTitle(sessionItem, fmt.Sprintf("Sessions: %d active", count))
	}
}

// setStatusTitle sets a menu item's label. dbusmenu treats "_" as an access
// key marker (so "input_0" would render as "input0" with an underlined 0),
// hence literal underscores have to be doubled.
func setStatusTitle(item *systray.MenuItem, title string) {
	item.SetTitle(escapeMenuLabel(title))
}

func escapeMenuLabel(label string) string {
	return strings.ReplaceAll(label, "_", "__")
}

func (d *Deej) stopTray() {
	d.logger.Debug("Quitting tray")
	systray.Quit()
}
