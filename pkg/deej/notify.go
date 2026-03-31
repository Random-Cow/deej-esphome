package deej

import (
	"os"
	"path/filepath"

	"github.com/gen2brain/beeep"
	"go.uber.org/zap"

	"github.com/Random-Cow/deej-esphome/pkg/deej/icon"
	"github.com/Random-Cow/deej-esphome/pkg/deej/util"
)

// Notifier provides generic notification sending
type Notifier interface {
	Notify(title string, message string)
}

// ToastNotifier sends desktop notifications via beeep (libnotify on Linux).
type ToastNotifier struct {
	logger      *zap.SugaredLogger
	appIconPath string
}

// NewToastNotifier creates a new ToastNotifier and writes the PNG icon to a temp file once.
func NewToastNotifier(logger *zap.SugaredLogger) (*ToastNotifier, error) {
	logger = logger.Named("notifier")

	appIconPath := filepath.Join(os.TempDir(), "deej.png")

	if !util.FileExists(appIconPath) {
		logger.Debugw("Writing notification icon to temp file", "path", appIconPath)

		iconData := icon.DeejLogoPNG
		if len(iconData) == 0 {
			iconData = icon.DeejLogo
		}

		f, err := os.Create(appIconPath)
		if err != nil {
			logger.Warnw("Failed to create notification icon file", "error", err)
			appIconPath = ""
		} else {
			if _, err = f.Write(iconData); err != nil {
				logger.Warnw("Failed to write notification icon", "error", err)
				appIconPath = ""
			}
			f.Close()
		}
	}

	tn := &ToastNotifier{
		logger:      logger,
		appIconPath: appIconPath,
	}

	logger.Debug("Created notifier instance")

	return tn, nil
}

// Notify sends a desktop notification using beeep (libnotify on Linux).
func (tn *ToastNotifier) Notify(title string, message string) {
	tn.logger.Infow("Sending desktop notification", "title", title, "message", message)

	if err := beeep.Notify(title, message, tn.appIconPath); err != nil {
		tn.logger.Errorw("Failed to send desktop notification", "error", err)
	}
}
