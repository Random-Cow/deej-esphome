package deej

import (
	"fyne.io/systray"

	"github.com/omriharel/deej/pkg/deej/icon"
	"github.com/omriharel/deej/pkg/deej/util"
)

// loadTrayIcon chooses PNG (Linux) if embedded, else falls back to ICO bytes
func loadTrayIcon() []byte {
	if util.Linux() && len(icon.DeejLogoPNG) > 0 {
		return icon.DeejLogoPNG
	}
	// Original ICO (works on Windows)
	if len(icon.DeejLogo) > 0 {
		return icon.DeejLogo
	}
	return nil
}

func (d *Deej) initializeTray(onDone func()) {
	logger := d.logger.Named("tray")

	onReady := func() {
		logger.Debug("Tray instance ready")

		// Load embedded icon (PNG preferred on Linux) with fallback
		iconData := loadTrayIcon()
		if len(iconData) == 0 {
			logger.Warn("Embedded icon data empty, falling back to ICO bytes")
			iconData = icon.DeejLogo
		}
		if len(iconData) == 0 {
			logger.Error("No valid icon data available; tray icon may not display")
		} else {
			systray.SetIcon(iconData)
		}
		systray.SetTitle("deej")
		systray.SetTooltip("deej")

		editConfig := systray.AddMenuItem("Edit configuration", "Open config file with notepad")
		// Skip setting icon for menu items for now to avoid format issues

		refreshSessions := systray.AddMenuItem("Re-scan audio sessions", "Manually refresh audio sessions if something's stuck")
		// Skip setting icon for menu items for now to avoid format issues

		if d.version != "" {
			systray.AddSeparator()
			versionInfo := systray.AddMenuItem(d.version, "")
			versionInfo.Disable()
		}

		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop deej and quit")

		// wait on things to happen
		go func() {
			for {
				select {

				// quit
				case <-quit.ClickedCh:
					logger.Info("Quit menu item clicked, stopping")

					d.signalStop()

				// edit config
				case <-editConfig.ClickedCh:
					logger.Info("Edit config menu item clicked, opening config for editing")

					editor := "notepad.exe"
					if util.Linux() {
						editor = "gedit"
					}

					if err := util.OpenExternal(logger, editor, userConfigFilepath); err != nil {
						logger.Warnw("Failed to open config file for editing", "error", err)
					}

				// refresh sessions
				case <-refreshSessions.ClickedCh:
					logger.Info("Refresh sessions menu item clicked, triggering session map refresh")

					// performance: the reason that forcing a refresh here is okay is that users can't spam the
					// right-click -> select-this-option sequence at a rate that's meaningful to performance
					d.sessions.refreshSessions(true)
				}
			}
		}()

		// actually start the main runtime
		onDone()
	}

	onExit := func() {
		logger.Debug("Tray exited")
	}

	// start the tray icon
	logger.Debug("Running in tray")
	systray.Run(onReady, onExit)
}

func (d *Deej) stopTray() {
	d.logger.Debug("Quitting tray")
	systray.Quit()
}
