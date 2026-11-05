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

		// actually start the main runtime
		onDone()
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
		connLine := fmt.Sprintf("ESPHome %s ✓ (up %s)", status.IPAddress, uptime)
		if status.ReconnectCount > 0 {
			connLine += fmt.Sprintf(", %d reconnect(s)", status.ReconnectCount)
		}
		connItem.SetTitle(connLine)
	} else {
		if status.ReconnectCount > 0 {
			connItem.SetTitle(fmt.Sprintf("ESPHome %s ✗ (reconnecting, attempt %d)", status.IPAddress, status.ReconnectCount+1))
		} else {
			connItem.SetTitle(fmt.Sprintf("ESPHome %s ✗ (connecting…)", status.IPAddress))
		}
	}

	// Slider values line
	sliderNames := d.config.ESPHomeSliderNames
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
		sliderItem.SetTitle("Sliders: " + strings.Join(parts, "  "))
	} else {
		sliderItem.SetTitle("Sliders: –")
	}

	// Session count line
	if d.sessions != nil {
		count := d.sessions.SessionCount()
		sessionItem.SetTitle(fmt.Sprintf("Sessions: %d active", count))
	}
}

func (d *Deej) stopTray() {
	d.logger.Debug("Quitting tray")
	systray.Quit()
}
