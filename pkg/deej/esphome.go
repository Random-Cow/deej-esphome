package deej

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/Random-Cow/deej-esphome/pkg/deej/util"
)

const (
	sseReconnectBaseDelay = time.Second
	sseReconnectMaxDelay  = time.Minute

	// ESPHome's web_server sends a "ping" event roughly every 10 seconds. If
	// nothing at all arrives for this long, the connection is considered dead
	// (e.g. the device lost power or WiFi without closing the TCP connection).
	sseIdleTimeout = 30 * time.Second

	// timeouts for establishing the HTTP connection. The stream itself has no
	// overall timeout since it's meant to stay open indefinitely.
	httpDialTimeout           = 5 * time.Second
	httpResponseHeaderTimeout = 5 * time.Second

	// how long Start() waits for the SSE stream's initial state replay before
	// returning, so callers can apply real slider values right after startup.
	initialSyncTimeout = 2 * time.Second
	initialSyncPoll    = 50 * time.Millisecond

	// how long Stop() waits for the SSE goroutine to wind down.
	stopTimeout = 2 * time.Second
)

// non-alphanumeric run matcher used to slugify ESPHome entity names the same
// way ESPHome derives object IDs from friendly names (e.g. "Input 0" -> "input_0").
var entityNameSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

// ESPHome manages the connection to an ESPHome device over HTTP/SSE.
type ESPHome struct {
	deej   *Deej
	logger *zap.SugaredLogger

	client      *http.Client
	idleTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// snapshot of the relevant config, refreshed on config reload. kept here
	// (under lock) so the SSE goroutine never races with the config reloader
	address     string
	sliderNames []string

	// cancels the current connection; used to reconnect when the address changes
	cancelConn   context.CancelFunc
	reconnectNow chan struct{}

	started        bool
	connected      bool
	connectedSince time.Time
	reconnectCount int

	currentSliderPercentValues []float32
	lock                       sync.Mutex

	sliderMoveConsumers []chan SliderMoveEvent
}

// ESPHomeStatus holds a snapshot of the current ESPHome connection state.
type ESPHomeStatus struct {
	Connected      bool
	Address        string
	SliderNames    []string
	ConnectedSince time.Time
	ReconnectCount int
	SliderValues   []float32
}

// sensorData represents an ESPHome sensor event from the SSE stream.
// NameId is only present on firmware built between ESPHome 2026.1.3 and 2026.8.0;
// on those versions it carries the new "domain/entity_name" format while Id still
// carries the legacy "domain-object_id" format, so it takes priority when present.
type sensorData struct {
	Id     string `json:"id"`
	NameId string `json:"name_id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	// Value is a number for sensors, but a bool or string for other entity
	// types (switches, text sensors, ...) that share the same stream.
	Value any `json:"value"`
}

// NewESPHome creates an ESPHome instance using the provided deej config.
func NewESPHome(deej *Deej, logger *zap.SugaredLogger) (*ESPHome, error) {
	logger = logger.Named("esphome")

	ctx, cancel := context.WithCancel(context.Background())

	esphome := &ESPHome{
		deej:   deej,
		logger: logger,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: preferIPv4Dial(&net.Dialer{
					Timeout:   httpDialTimeout,
					KeepAlive: 15 * time.Second,
				}),
				ResponseHeaderTimeout: httpResponseHeaderTimeout,
			},
		},
		idleTimeout:                sseIdleTimeout,
		ctx:                        ctx,
		cancel:                     cancel,
		done:                       make(chan struct{}),
		reconnectNow:               make(chan struct{}, 1),
		address:                    deej.config.ESPHomeAddress,
		sliderNames:                append([]string(nil), deej.config.ESPHomeSliderNames...),
		currentSliderPercentValues: newSliderValues(len(deej.config.ESPHomeSliderNames)),
		sliderMoveConsumers:        []chan SliderMoveEvent{},
	}

	esphome.setupOnConfigReload()

	logger.Debug("Created ESPHome instance")

	return esphome, nil
}

// Start begins the SSE stream loop and waits briefly for its initial state
// replay (which ESPHome sends automatically on every /events connection) so
// callers can apply real slider values right after Start returns. It gives up
// after initialSyncTimeout if the device is unreachable; the SSE loop keeps
// retrying in the background regardless.
func (esphome *ESPHome) Start() error {
	esphome.lock.Lock()
	if esphome.started {
		esphome.lock.Unlock()
		esphome.logger.Warn("Already started, can't start another without closing first")
		return errors.New("ESPHome: connection already active")
	}
	esphome.started = true
	esphome.lock.Unlock()

	go esphome.sseLoop()

	deadline := time.Now().Add(initialSyncTimeout)
	for time.Now().Before(deadline) && esphome.ctx.Err() == nil {
		if esphome.hasReceivedInitialValues() {
			break
		}
		time.Sleep(initialSyncPoll)
	}

	return nil
}

// hasReceivedInitialValues reports whether at least one slider has moved past
// its sentinel value, meaning the SSE stream's initial state replay arrived.
func (esphome *ESPHome) hasReceivedInitialValues() bool {
	esphome.lock.Lock()
	defer esphome.lock.Unlock()

	for _, v := range esphome.currentSliderPercentValues {
		if v >= 0 {
			return true
		}
	}
	return false
}

// Stop shuts down the SSE connection and waits (briefly) for it to close.
// It's safe to call even if Start was never called, and more than once.
func (esphome *ESPHome) Stop() {
	esphome.cancel()

	esphome.lock.Lock()
	started := esphome.started
	esphome.lock.Unlock()

	if !started {
		return
	}

	select {
	case <-esphome.done:
	case <-time.After(stopTimeout):
		esphome.logger.Warn("Timed out waiting for SSE stream to close")
	}
}

// SubscribeToSliderMoveEvents returns a channel that receives a SliderMoveEvent
// every time a slider changes value beyond the noise-reduction threshold.
func (esphome *ESPHome) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	esphome.sliderMoveConsumers = append(esphome.sliderMoveConsumers, ch)
	return ch
}

// GetCurrentSliderValues returns a snapshot of the last-known normalised slider values.
func (esphome *ESPHome) GetCurrentSliderValues() []float32 {
	esphome.lock.Lock()
	defer esphome.lock.Unlock()

	result := make([]float32, len(esphome.currentSliderPercentValues))
	copy(result, esphome.currentSliderPercentValues)
	return result
}

// GetStatus returns a snapshot of the current ESPHome connection state for display.
func (esphome *ESPHome) GetStatus() ESPHomeStatus {
	esphome.lock.Lock()
	defer esphome.lock.Unlock()

	values := make([]float32, len(esphome.currentSliderPercentValues))
	copy(values, esphome.currentSliderPercentValues)

	return ESPHomeStatus{
		Connected:      esphome.connected,
		Address:        esphome.address,
		SliderNames:    append([]string(nil), esphome.sliderNames...),
		ConnectedSince: esphome.connectedSince,
		ReconnectCount: esphome.reconnectCount,
		SliderValues:   values,
	}
}

func (esphome *ESPHome) setupOnConfigReload() {
	configReloadedChannel := esphome.deej.config.SubscribeToChanges()

	go func() {
		for range configReloadedChannel {
			esphome.applyConfig(esphome.deej.config.ESPHomeAddress, esphome.deej.config.ESPHomeSliderNames)
		}
	}()
}

// applyConfig takes over a (re)loaded config. Slider values are reset so
// move events are re-emitted and the session map receives fresh volumes; if
// the device address changed, the current connection is dropped and a new
// one is opened right away.
func (esphome *ESPHome) applyConfig(address string, sliderNames []string) {
	esphome.lock.Lock()
	addressChanged := address != esphome.address
	esphome.address = address
	esphome.sliderNames = append([]string(nil), sliderNames...)
	esphome.currentSliderPercentValues = newSliderValues(len(sliderNames))
	cancelConn := esphome.cancelConn
	esphome.lock.Unlock()

	if !addressChanged {
		return
	}

	esphome.logger.Infow("ESPHome address changed, reconnecting", "address", address)

	select {
	case esphome.reconnectNow <- struct{}{}:
	default:
	}
	if cancelConn != nil {
		cancelConn()
	}
}

// newSliderValues returns n slider values set to the "unknown" sentinel, which
// forces an event on the first real value.
func newSliderValues(n int) []float32 {
	values := make([]float32, n)
	for i := range values {
		values[i] = -1.0
	}
	return values
}

// preferIPv4Dial resolves host names over IPv4 first and only falls back to a
// regular (dual-stack) dial if that fails. Go's built-in resolver waits for
// both A and AAAA answers, and mDNS names of devices without IPv6 (typical
// for ESPHome) never get an AAAA answer, which turns every connect into a
// multi-second timeout.
func preferIPv4Dial(dialer *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || net.ParseIP(host) != nil || network != "tcp" {
			return dialer.DialContext(ctx, network, address)
		}

		conn, err := dialer.DialContext(ctx, "tcp4", address)
		var dnsErr *net.DNSError
		if err != nil && errors.As(err, &dnsErr) && ctx.Err() == nil {
			return dialer.DialContext(ctx, network, address)
		}
		return conn, err
	}
}

// eventsURL builds the SSE endpoint URL from the configured address, which may
// be an IP, a hostname (e.g. "deej.local"), "host:port", or include "http://".
func eventsURL(address string) string {
	address = strings.TrimSpace(address)
	address = strings.TrimPrefix(address, "http://")
	address = strings.TrimSuffix(address, "/")
	return fmt.Sprintf("http://%s/events", address)
}

// sseLoop maintains the SSE connection, reconnecting with exponential backoff on failure.
func (esphome *ESPHome) sseLoop() {
	defer close(esphome.done)

	backoff := sseReconnectBaseDelay

	for {
		esphome.lock.Lock()
		address := esphome.address
		esphome.lock.Unlock()

		logger := esphome.logger.Named(address)

		wasConnected, err := esphome.runSSEStream(esphome.ctx, logger, address)

		esphome.lock.Lock()
		esphome.connected = false
		addressChanged := esphome.address != address
		if esphome.ctx.Err() == nil && !addressChanged {
			esphome.reconnectCount++
		}
		if addressChanged {
			esphome.reconnectCount = 0
		}
		esphome.lock.Unlock()

		// the config now points somewhere else: connect there right away
		if addressChanged && esphome.ctx.Err() == nil {
			select {
			case <-esphome.reconnectNow:
			default:
			}
			backoff = sseReconnectBaseDelay
			continue
		}

		if esphome.ctx.Err() != nil {
			logger.Debug("SSE loop stopped")
			return
		}

		// a connection that actually worked resets the backoff, so a single
		// hiccup after hours of uptime doesn't inherit an old, long delay
		if wasConnected {
			backoff = sseReconnectBaseDelay
		}

		logger.Warnw("SSE stream ended, reconnecting", "error", err, "backoff", backoff)

		select {
		case <-esphome.ctx.Done():
			logger.Debug("SSE loop stopped")
			return
		case <-esphome.reconnectNow:
			backoff = sseReconnectBaseDelay
			continue
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > sseReconnectMaxDelay {
			backoff = sseReconnectMaxDelay
		}
	}
}

// runSSEStream opens GET /events and processes the Server-Sent Events stream.
// It returns when the stream ends, an error occurs, the stream goes silent for
// longer than the idle timeout, or ctx is cancelled. wasConnected reports
// whether the HTTP connection was established successfully.
func (esphome *ESPHome) runSSEStream(ctx context.Context, logger *zap.SugaredLogger, address string) (wasConnected bool, err error) {
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	esphome.lock.Lock()
	esphome.cancelConn = cancelConn
	esphome.lock.Unlock()
	defer func() {
		esphome.lock.Lock()
		esphome.cancelConn = nil
		esphome.lock.Unlock()
	}()

	req, err := http.NewRequestWithContext(connCtx, http.MethodGet, eventsURL(address), nil)
	if err != nil {
		return false, fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := esphome.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("connect to SSE: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("connect to SSE: unexpected HTTP status %s (is web_server enabled?)", resp.Status)
	}

	esphome.lock.Lock()
	esphome.connected = true
	esphome.connectedSince = time.Now()
	esphome.lock.Unlock()

	logger.Infow("Connected to ESPHome SSE stream")

	// tear the connection down if the device goes silent
	watchdog := time.AfterFunc(esphome.idleTimeout, cancelConn)
	defer watchdog.Stop()

	scanner := bufio.NewScanner(resp.Body)
	var eventType string

	for scanner.Scan() {
		watchdog.Reset(esphome.idleTimeout)

		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")

		case strings.HasPrefix(line, "data: ") && eventType == "state":
			dataStr := strings.TrimPrefix(line, "data: ")
			var data sensorData
			if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
				logger.Warnw("Failed to parse SSE data", "data", dataStr, "error", err)
				eventType = ""
				continue
			}
			id := data.NameId
			if id == "" {
				id = data.Id
			}
			if value, isNumber := data.Value.(float64); isNumber {
				if idx := esphome.findSliderIndex(id); idx >= 0 {
					esphome.updateSlider(idx, value)
				}
			}
			eventType = ""

		case line == "":
			eventType = ""
		}
	}

	switch {
	case ctx.Err() != nil:
		return true, ctx.Err()
	case connCtx.Err() != nil:
		return true, fmt.Errorf("no data received for %s, assuming connection is dead", esphome.idleTimeout)
	case scanner.Err() != nil:
		return true, fmt.Errorf("SSE stream read error: %w", scanner.Err())
	}

	return true, errors.New("SSE stream closed by server")
}

// findSliderIndex maps an ESPHome entity id to a slider index. It accepts both
// the legacy "sensor-object_id" format and the current "sensor/Entity Name"
// (or "sensor/device/Entity Name") format introduced in ESPHome 2026.8.0.
func (esphome *ESPHome) findSliderIndex(id string) int {
	slug := sanitizeEntityName(id)
	if slashIdx := strings.LastIndex(id, "/"); slashIdx >= 0 {
		slug = sanitizeEntityName(id[slashIdx+1:])
	} else if strings.HasPrefix(id, "sensor-") {
		slug = strings.TrimPrefix(id, "sensor-")
	}

	esphome.lock.Lock()
	defer esphome.lock.Unlock()

	for i, name := range esphome.sliderNames {
		if slug == name {
			return i
		}
	}
	return -1
}

// sanitizeEntityName slugifies an ESPHome entity name the way ESPHome derives
// object IDs from friendly names, e.g. "Input 0" -> "input_0".
func sanitizeEntityName(name string) string {
	slug := entityNameSanitizer.ReplaceAllString(strings.ToLower(name), "_")
	return strings.Trim(slug, "_")
}

// updateSlider converts a raw sensor value to a normalised volume scalar,
// applies noise reduction, and emits a SliderMoveEvent if the value changed.
func (esphome *ESPHome) updateSlider(idx int, rawValue float64) {
	// ESPHome ADC sensors report 0–100 (native percent)
	dirtyFloat := float32(rawValue) / 100.0
	normalizedScalar := util.NormalizeScalar(dirtyFloat)

	if esphome.deej.config.InvertSliders {
		normalizedScalar = 1 - normalizedScalar
	}

	esphome.lock.Lock()
	if idx < 0 || idx >= len(esphome.currentSliderPercentValues) {
		esphome.lock.Unlock()
		return
	}

	prev := esphome.currentSliderPercentValues[idx]
	shouldEmit := util.SignificantlyDifferent(prev, normalizedScalar, esphome.deej.config.NoiseReductionLevel)
	if shouldEmit {
		esphome.currentSliderPercentValues[idx] = normalizedScalar
	}
	esphome.lock.Unlock()

	if !shouldEmit {
		return
	}

	event := SliderMoveEvent{
		SliderID:     idx,
		PercentValue: normalizedScalar,
	}

	if esphome.deej.Verbose() {
		esphome.logger.Debugw("Slider moved", "event", event)
	}

	for _, consumer := range esphome.sliderMoveConsumers {
		consumer <- event
	}
}
