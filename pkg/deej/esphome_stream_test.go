package deej

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestESPHome(t *testing.T, address string, sliders ...string) *ESPHome {
	t.Helper()

	d := &Deej{
		logger: zap.NewNop().Sugar(),
		config: &CanonicalConfig{
			ESPHomeAddress:      address,
			ESPHomeSliderNames:  sliders,
			NoiseReductionLevel: "low",
			logger:              zap.NewNop().Sugar(),
		},
	}

	esphome, err := NewESPHome(d, d.logger)
	if err != nil {
		t.Fatalf("NewESPHome: %v", err)
	}
	t.Cleanup(esphome.Stop)

	return esphome
}

func TestUpdateSliderNormalization(t *testing.T) {
	cases := []struct {
		name   string
		raw    float64
		invert bool
		want   float32
	}{
		{"zero", 0, false, 0},
		{"half", 50, false, 0.5},
		{"full", 100, false, 1},
		{"inverted full", 100, true, 0},
		{"inverted quarter", 25, true, 0.75},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			esphome := newTestESPHome(t, "127.0.0.1", "input_0")
			esphome.deej.config.InvertSliders = c.invert

			esphome.updateSlider(0, c.raw)

			if got := esphome.GetCurrentSliderValues()[0]; got != c.want {
				t.Errorf("updateSlider(%v) stored %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

func TestUpdateSliderIgnoresNoise(t *testing.T) {
	esphome := newTestESPHome(t, "127.0.0.1", "input_0")
	events := esphome.SubscribeToSliderMoveEvents()

	received := make(chan SliderMoveEvent, 10)
	go func() {
		for e := range events {
			received <- e
		}
	}()

	esphome.updateSlider(0, 50)
	esphome.updateSlider(0, 50.4) // below every noise threshold
	esphome.updateSlider(0, 80)

	want := []float32{0.5, 0.8}
	for _, w := range want {
		select {
		case e := <-received:
			if e.PercentValue != w {
				t.Errorf("got event %v, want %v", e.PercentValue, w)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %v", w)
		}
	}

	select {
	case e := <-received:
		t.Errorf("unexpected extra event %v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventsURL(t *testing.T) {
	cases := map[string]string{
		"192.168.1.50":       "http://192.168.1.50/events",
		"deej.local":         "http://deej.local/events",
		"deej.local:8080":    "http://deej.local:8080/events",
		"http://deej.local/": "http://deej.local/events",
		"  192.168.1.50  ":   "http://192.168.1.50/events",
		"http://10.0.0.2:80": "http://10.0.0.2:80/events",
	}

	for in, want := range cases {
		if got := eventsURL(in); got != want {
			t.Errorf("eventsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// sseServer serves an ESPHome-like /events stream: the given frames, then it
// keeps the connection open until the client goes away.
func sseServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush() // send headers right away, like ESPHome does
		for _, f := range frames {
			_, _ = fmt.Fprint(w, f)
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	return srv
}

func stateFrame(json string) string {
	return "event: state\ndata: " + json + "\n\n"
}

func TestRunSSEStreamParsesStateEvents(t *testing.T) {
	srv := sseServer(t,
		"retry: 30000\nid: 1\nevent: ping\ndata: {}\n\n",
		stateFrame(`{"id":"sensor-input_0","value":25,"state":"25"}`),
		stateFrame(`{"id":"sensor-input_1","name_id":"sensor/Input 1","value":75,"state":"75"}`),
		"event: state\ndata: {not json\n\n",
		stateFrame(`{"id":"switch/Projector Power","value":false,"state":"OFF"}`),
		stateFrame(`{"id":"text_sensor/Input 0","value":"POW=OFF","state":"POW=OFF"}`),
		stateFrame(`{"id":"sensor/WiFi Signal","value":-60}`),
	)

	esphome := newTestESPHome(t, strings.TrimPrefix(srv.URL, "http://"), "input_0", "input_1")

	go func() { _, _ = esphome.runSSEStream(esphome.ctx, esphome.logger, esphome.address) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v := esphome.GetCurrentSliderValues()
		if v[0] == 0.25 && v[1] == 0.75 {
			if !esphome.GetStatus().Connected {
				t.Error("expected status to be connected")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slider values = %v, want [0.25 0.75]", esphome.GetCurrentSliderValues())
}

func TestRunSSEStreamRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	esphome := newTestESPHome(t, strings.TrimPrefix(srv.URL, "http://"), "input_0")

	wasConnected, err := esphome.runSSEStream(context.Background(), esphome.logger, esphome.address)
	if wasConnected || err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got wasConnected=%v err=%v, want a 404 error", wasConnected, err)
	}
}

func TestRunSSEStreamIdleTimeout(t *testing.T) {
	srv := sseServer(t) // connects, then never sends anything

	esphome := newTestESPHome(t, strings.TrimPrefix(srv.URL, "http://"), "input_0")
	esphome.idleTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := esphome.runSSEStream(context.Background(), esphome.logger, esphome.address)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no data received") {
			t.Fatalf("got err=%v, want idle timeout error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runSSEStream did not give up on a silent connection")
	}
}

// Regression test: Stop used to block forever while the stream was idle.
func TestStopWhileConnectedReturnsPromptly(t *testing.T) {
	srv := sseServer(t, stateFrame(`{"id":"sensor-input_0","value":10}`))

	esphome := newTestESPHome(t, strings.TrimPrefix(srv.URL, "http://"), "input_0")
	if err := esphome.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		esphome.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return")
	}

	select {
	case <-esphome.done:
	default:
		t.Fatal("SSE loop still running after Stop")
	}
}

// Changing the address in the config must move the live connection over to
// the new device without a restart.
func TestAddressChangeReconnects(t *testing.T) {
	oldDevice := sseServer(t, stateFrame(`{"id":"sensor-input_0","value":10}`))
	newDevice := sseServer(t, stateFrame(`{"id":"sensor-input_0","value":90}`))

	esphome := newTestESPHome(t, strings.TrimPrefix(oldDevice.URL, "http://"), "input_0")
	if err := esphome.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForSlider(t, esphome, 0, 0.1)

	cfg := esphome.deej.config
	cfg.ESPHomeAddress = strings.TrimPrefix(newDevice.URL, "http://")
	cfg.ESPHomeSliderNames = []string{"input_0", "input_1"}
	cfg.onConfigReloaded()

	waitForSlider(t, esphome, 0, 0.9)

	status := esphome.GetStatus()
	if status.Address != cfg.ESPHomeAddress {
		t.Errorf("status address = %q, want %q", status.Address, cfg.ESPHomeAddress)
	}
	if len(status.SliderValues) != 2 {
		t.Errorf("got %d slider values after reload, want 2", len(status.SliderValues))
	}
}

func waitForSlider(t *testing.T, esphome *ESPHome, idx int, want float32) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v := esphome.GetCurrentSliderValues(); idx < len(v) && v[idx] == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slider %d = %v, want %v", idx, esphome.GetCurrentSliderValues(), want)
}

func TestPreferIPv4DialUsesIPv4ForHostnames(t *testing.T) {
	srv := sseServer(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")

	var networks []string
	dial := preferIPv4Dial(&net.Dialer{Timeout: time.Second})
	recording := func(ctx context.Context, network, address string) (net.Conn, error) {
		networks = append(networks, network)
		return dial(ctx, network, address)
	}

	conn, err := recording(context.Background(), "tcp", "localhost:"+port)
	if err != nil {
		t.Fatalf("dial localhost: %v", err)
	}
	_ = conn.Close()

	if len(networks) != 1 || networks[0] != "tcp" {
		t.Fatalf("unexpected outer calls %v", networks)
	}
}

func TestStopWithoutStart(t *testing.T) {
	esphome := newTestESPHome(t, "127.0.0.1", "input_0")
	esphome.Stop()
	esphome.Stop()
}
