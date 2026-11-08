package deej

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/thoas/go-funk"
	"go.uber.org/zap"
)

type sessionMap struct {
	deej   *Deej
	logger *zap.SugaredLogger

	m    map[string][]Session
	lock sync.Locker

	sessionFinder SessionFinder

	lastSessionRefresh time.Time
	unmappedSessions   []Session

	// lastApplyTime tracks when we last called applyCurrentSliderVolumes.
	// Used to debounce SINK_INPUT_CHANGE events caused by our own SetVolume calls.
	lastApplyTime time.Time
}

const (
	masterSessionName = "master" // master device volume
	systemSessionName = "system" // system sounds volume
	inputSessionName  = "mic"    // microphone input level

	// some targets need to be transformed before their correct audio sessions can be accessed.
	// this prefix identifies those targets to ensure they don't contradict with another similarly-named process
	specialTargetTransformPrefix = "deej."

	// targets all currently unmapped sessions (experimental)
	specialTargetAllUnmapped = "unmapped"

	// this threshold constant assumes that re-acquiring all sessions is a kind of expensive operation,
	// and needs to be limited in some manner. this value was previously user-configurable through a config
	// key "process_refresh_frequency", but exposing this type of implementation detail seems wrong now
	minTimeBetweenSessionRefreshes = time.Second * 5

	// determines whether the map should be refreshed when a slider moves.
	// this is a bit greedy but allows us to ensure sessions are always re-acquired, which is
	// especially important for process groups (because you can have one ongoing session
	// always preventing lookup of other processes bound to its slider, which forces the user
	// to manually refresh sessions). a cleaner way to do this down the line is by registering to notifications
	// whenever a new session is added, but that's too hard to justify for how easy this solution is
	maxTimeBetweenSessionRefreshes = time.Second * 45

	// how long after our own SetVolume call we ignore incoming CHANGE events.
	// This breaks the feedback loop: our SetVolume causes a CHANGE event within ~5ms;
	// external overrides (Firefox, WirePlumber) typically arrive well after this window.
	changeEventDebounce = 150 * time.Millisecond

	// backoff values for PA reconnection attempts
	paReconnectBaseDelay = time.Second
	paReconnectMaxDelay  = 30 * time.Second
)

// this matches friendly device names, e.g. "Headphones (Realtek Audio)"
var deviceSessionKeyPattern = regexp.MustCompile(`^.+ \(.+\)$`)

func newSessionMap(deej *Deej, logger *zap.SugaredLogger, sessionFinder SessionFinder) (*sessionMap, error) {
	logger = logger.Named("sessions")

	m := &sessionMap{
		deej:          deej,
		logger:        logger,
		m:             make(map[string][]Session),
		lock:          &sync.Mutex{},
		sessionFinder: sessionFinder,
	}

	logger.Debug("Created session map instance")

	return m, nil
}

func (m *sessionMap) initialize() error {
	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to get all sessions during session map initialization", "error", err)
		return fmt.Errorf("get all sessions during init: %w", err)
	}

	m.setupOnConfigReload()
	m.setupOnSliderMove()
	m.setupOnNewSession()
	m.setupOnSessionChange()

	return nil
}

func (m *sessionMap) release() error {
	if err := m.sessionFinder.Release(); err != nil {
		m.logger.Warnw("Failed to release session finder during session map release", "error", err)
		return fmt.Errorf("release session finder during release: %w", err)
	}

	return nil
}

// assumes the session map is clean!
// only call on a new session map or as part of refreshSessions which calls reset
func (m *sessionMap) getAndAddSessions() error {

	// mark that we're refreshing before anything else
	m.lastSessionRefresh = time.Now()
	m.unmappedSessions = nil

	sessions, err := m.sessionFinder.GetAllSessions()
	if err != nil {
		m.logger.Warnw("Failed to get sessions from session finder", "error", err)
		return fmt.Errorf("get sessions from SessionFinder: %w", err)
	}

	for _, session := range sessions {
		m.add(session)

		if !m.sessionMapped(session) {
			m.logger.Debugw("Tracking unmapped session", "session", session)
			m.unmappedSessions = append(m.unmappedSessions, session)
		}
	}

	m.logger.Infow("Got all audio sessions successfully", "sessionMap", m)

	return nil
}

func (m *sessionMap) setupOnConfigReload() {
	configReloadedChannel := m.deej.config.SubscribeToChanges()

	go func() {
		for range configReloadedChannel {
			m.logger.Info("Detected config reload, attempting to re-acquire all audio sessions")
			m.refreshSessions(false)
		}
	}()
}

func (m *sessionMap) setupOnSliderMove() {
	sliderEventsChannel := m.deej.sliderEventSource.SubscribeToSliderMoveEvents()

	go func() {
		for event := range sliderEventsChannel {
			m.handleSliderMoveEvent(event)
		}
	}()
}

func (m *sessionMap) setupOnNewSession() {
	newSessionCh := m.sessionFinder.SubscribeToNewSessions()

	go func() {
		for range newSessionCh {
			m.logger.Debug("New audio session detected via PulseAudio event, refreshing and applying volumes")
			m.refreshSessions(true)
			m.ApplyCurrentSliderVolumes()

			// Some apps (Firefox, Spotify, WirePlumber) set or restore their own
			// stream volume asynchronously after stream creation. Re-apply our
			// desired volumes at increasing delays to ensure we win the race.
			for _, delay := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond, 1000 * time.Millisecond} {
				go func(d time.Duration) {
					time.Sleep(d)
					m.ApplyCurrentSliderVolumes()
				}(delay)
			}
		}
	}()
}

// setupOnSessionChange listens for SINK_INPUT_CHANGE events and re-applies
// slider volumes whenever an external source (Firefox, Spotify, WirePlumber)
// overrides a volume that deej previously set.
func (m *sessionMap) setupOnSessionChange() {
	changedCh := m.sessionFinder.SubscribeToSessionChanges()

	go func() {
		for range changedCh {
			// Debounce: ignore CHANGE events that arrive shortly after our own
			// SetVolume calls (those changes are caused by us, not external software).
			if time.Since(m.lastApplyTime) < changeEventDebounce {
				continue
			}

			m.logger.Debug("External volume change detected, re-applying slider volumes")
			m.ApplyCurrentSliderVolumes()
		}
	}()
}

// applyCurrentSliderVolumes sets the volume of every mapped session to the
// current slider position. It is called immediately after a new audio session
// appears and whenever an external volume change is detected, to ensure deej
// always wins against app-initiated or WirePlumber-initiated overrides.
func (m *sessionMap) ApplyCurrentSliderVolumes() {
	currentValues := m.deej.sliderEventSource.GetCurrentSliderValues()
	if len(currentValues) == 0 {
		return
	}

	// Record the time so CHANGE events caused by our own SetVolume calls can be
	// identified and ignored by setupOnSessionChange (debounce).
	m.lastApplyTime = time.Now()

	m.deej.config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		if sliderIdx >= len(currentValues) {
			return
		}

		volume := currentValues[sliderIdx]

		// Skip sentinel value (-1.0): ESPHome hasn't delivered a real value yet.
		if volume < 0 {
			return
		}

		for _, target := range targets {
			resolvedTargets := m.resolveTarget(target)
			for _, resolvedTarget := range resolvedTargets {
				sessions, ok := m.getMatching(resolvedTarget)
				if !ok {
					continue
				}
				for _, session := range sessions {
					if err := session.SetVolume(volume); err != nil {
						m.logger.Warnw("Failed to apply current volume to session", "error", err)
					}
				}
			}
		}
	})
}

// performance: explain why force == true at every such use to avoid unintended forced refresh spams
func (m *sessionMap) refreshSessions(force bool) {

	// make sure enough time passed since the last refresh, unless force is true in which case always clear
	if !force && m.lastSessionRefresh.Add(minTimeBetweenSessionRefreshes).After(time.Now()) {
		return
	}

	// clear and release sessions first
	m.clear()

	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to re-acquire all audio sessions", "error", err)
		// Trigger a background reconnect attempt; the next slider event or
		// periodic check will succeed once PipeWire is back.
		go m.reconnectSessionFinder()
	} else {
		m.logger.Debug("Re-acquired sessions successfully")
	}
}

// reconnectSessionFinder releases the current (broken) PA connection and
// re-establishes it with exponential backoff. Called when GetAllSessions fails,
// typically after PipeWire restarts.
func (m *sessionMap) reconnectSessionFinder() {
	m.logger.Info("Attempting to reconnect to PipeWire/PulseAudio")

	// Release the broken connection (ignore error — it's already dead).
	m.sessionFinder.Release() //nolint:errcheck

	backoff := paReconnectBaseDelay

	for {
		time.Sleep(backoff)

		newFinder, err := newSessionFinder(m.deej.logger)
		if err != nil {
			m.logger.Warnw("PA reconnect attempt failed, retrying", "error", err, "backoff", backoff)
			backoff *= 2
			if backoff > paReconnectMaxDelay {
				backoff = paReconnectMaxDelay
			}
			continue
		}

		m.logger.Info("Reconnected to PipeWire/PulseAudio successfully")
		m.sessionFinder = newFinder

		// Re-subscribe internal goroutines to the new finder's channels.
		m.setupOnNewSession()
		m.setupOnSessionChange()

		// Repopulate sessions with the new connection.
		m.clear()
		if err := m.getAndAddSessions(); err != nil {
			m.logger.Warnw("Failed to get sessions after PA reconnect", "error", err)
		} else {
			m.ApplyCurrentSliderVolumes()
		}

		return
	}
}

// returns true if a session is not currently mapped to any slider, false otherwise
// special sessions (master, system, mic) and device-specific sessions always count as mapped,
// even when absent from the config. this makes sense for every current feature that uses "unmapped sessions"
func (m *sessionMap) sessionMapped(session Session) bool {

	// count master/system/mic as mapped
	if funk.ContainsString([]string{masterSessionName, systemSessionName, inputSessionName}, session.Key()) {
		return true
	}

	// count device sessions as mapped
	if deviceSessionKeyPattern.MatchString(session.Key()) {
		return true
	}

	matchFound := false

	// look through the actual mappings
	m.deej.config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		for _, target := range targets {

			// ignore special transforms
			if m.targetHasSpecialTransform(target) {
				continue
			}

			// safe to assume this has a single element because we made sure there's no special transform
			target = m.resolveTarget(target)[0]

			if target == session.Key() {
				matchFound = true
				return
			}

			// Also support glob patterns (e.g. "firefox*" matches "firefox", "Web Content", etc.)
			if isGlobPattern(target) {
				if matched, _ := path.Match(target, session.Key()); matched {
					matchFound = true
					return
				}
			}
		}
	})

	return matchFound
}

func (m *sessionMap) handleSliderMoveEvent(event SliderMoveEvent) {

	// first of all, ensure our session map isn't moldy
	if m.lastSessionRefresh.Add(maxTimeBetweenSessionRefreshes).Before(time.Now()) {
		m.logger.Debug("Stale session map detected on slider move, refreshing")
		m.refreshSessions(true)
	}

	// get the targets mapped to this slider from the config
	targets, ok := m.deej.config.SliderMapping.get(event.SliderID)

	// if slider not found in config, silently ignore
	if !ok {
		return
	}

	targetFound := false
	adjustmentFailed := false

	// for each possible target for this slider...
	for _, target := range targets {

		// resolve the target name by cleaning it up and applying any special transformations.
		// depending on the transformation applied, this can result in more than one target name
		resolvedTargets := m.resolveTarget(target)

		// for each resolved target...
		for _, resolvedTarget := range resolvedTargets {

			// check the map for matching sessions (supports glob patterns)
			sessions, ok := m.getMatching(resolvedTarget)

			// no sessions matching this target - move on
			if !ok {
				continue
			}

			targetFound = true

			// iterate all matching sessions and adjust the volume of each one
			for _, session := range sessions {
				if session.GetVolume() != event.PercentValue {
					// Record apply time before SetVolume so the resulting
					// SINK_INPUT_CHANGE event is treated as our own and ignored.
					m.lastApplyTime = time.Now()
					if err := session.SetVolume(event.PercentValue); err != nil {
						m.logger.Warnw("Failed to set target session volume", "error", err)
						adjustmentFailed = true
					}
				}
			}
		}
	}

	// if we still haven't found a target or the volume adjustment failed, maybe look for the target again.
	// processes could've opened since the last time this slider moved.
	// if they haven't, the cooldown will take care to not spam it up
	if !targetFound {
		m.refreshSessions(false)
	} else if adjustmentFailed {

		// performance: the reason that forcing a refresh here is okay is that we'll only get here
		// when a session's SetVolume call errored, such as in the case of a stale master session
		// (or another, more catastrophic failure happens)
		m.refreshSessions(true)
	}
}

func (m *sessionMap) targetHasSpecialTransform(target string) bool {
	return strings.HasPrefix(target, specialTargetTransformPrefix)
}

func (m *sessionMap) resolveTarget(target string) []string {

	// start by ignoring the case
	target = strings.ToLower(target)

	// look for any special targets first, by examining the prefix
	if m.targetHasSpecialTransform(target) {
		return m.applyTargetTransform(strings.TrimPrefix(target, specialTargetTransformPrefix))
	}

	return []string{target}
}

func (m *sessionMap) applyTargetTransform(specialTargetName string) []string {

	// select the transformation based on its name
	switch specialTargetName {

	// get currently unmapped sessions
	case specialTargetAllUnmapped:
		targetKeys := make([]string, len(m.unmappedSessions))
		for sessionIdx, session := range m.unmappedSessions {
			targetKeys[sessionIdx] = session.Key()
		}

		return targetKeys
	}

	return nil
}

func (m *sessionMap) add(value Session) {
	m.lock.Lock()
	defer m.lock.Unlock()

	key := value.Key()

	existing, ok := m.m[key]
	if !ok {
		m.m[key] = []Session{value}
	} else {
		m.m[key] = append(existing, value)
	}
}

func (m *sessionMap) get(key string) ([]Session, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	value, ok := m.m[key]
	return value, ok
}

// getMatching returns all sessions whose key exactly matches the target, or
// if the target is a glob pattern, all sessions whose key matches the pattern.
func (m *sessionMap) getMatching(target string) ([]Session, bool) {
	// exact match first: session keys can themselves contain glob
	// metacharacters (e.g. "[...]"), and deej.unmapped resolves to raw keys,
	// which would otherwise be misread as patterns that don't match themselves.
	if sessions, ok := m.get(target); ok || !isGlobPattern(target) {
		return sessions, ok
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	var result []Session
	for key, sessions := range m.m {
		if matched, _ := path.Match(target, key); matched {
			result = append(result, sessions...)
		}
	}

	return result, len(result) > 0
}

// isGlobPattern returns true if s contains any glob metacharacters.
func isGlobPattern(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

func (m *sessionMap) clear() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.logger.Debug("Releasing and clearing all audio sessions")

	for key, sessions := range m.m {
		for _, session := range sessions {
			session.Release()
		}

		delete(m.m, key)
	}

	m.logger.Debug("Session map cleared")
}

func (m *sessionMap) String() string {
	m.lock.Lock()
	defer m.lock.Unlock()

	sessionCount := 0

	for _, value := range m.m {
		sessionCount += len(value)
	}

	return fmt.Sprintf("<%d audio sessions>", sessionCount)
}

// SessionCount returns the total number of currently tracked audio sessions.
func (m *sessionMap) SessionCount() int {
	m.lock.Lock()
	defer m.lock.Unlock()

	count := 0
	for _, sessions := range m.m {
		count += len(sessions)
	}
	return count
}
