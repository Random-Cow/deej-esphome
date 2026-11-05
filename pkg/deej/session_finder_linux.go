package deej

import (
	"fmt"
	"net"

	"github.com/jfreymuth/pulse/proto"
	"go.uber.org/zap"
)

type paSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	client *proto.Client
	conn   net.Conn

	newSinkInputCh     chan uint32
	changedSinkInputCh chan uint32
}

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	client, conn, err := proto.Connect("")
	if err != nil {
		logger.Warnw("Failed to establish PulseAudio connection", "error", err)
		return nil, fmt.Errorf("establish PulseAudio connection: %w", err)
	}

	request := proto.SetClientName{
		Props: proto.PropList{
			"application.name": proto.PropListString("deej"),
		},
	}
	reply := proto.SetClientNameReply{}

	if err := client.Request(&request, &reply); err != nil {
		return nil, err
	}

	sf := &paSessionFinder{
		logger:             logger.Named("session_finder"),
		sessionLogger:      logger.Named("sessions"),
		client:             client,
		conn:               conn,
		newSinkInputCh:     make(chan uint32, 32),
		changedSinkInputCh: make(chan uint32, 64),
	}

	// Subscribe to sink-input events (new, change, remove) so we can react
	// immediately and fight back when other software overrides our volumes.
	if err := client.Request(&proto.Subscribe{Mask: proto.SubscriptionMaskSinkInput}, nil); err != nil {
		sf.logger.Warnw("Failed to subscribe to PulseAudio sink-input events, volume-spike prevention disabled", "error", err)
	} else {
		client.Callback = func(msg interface{}) {
			event, ok := msg.(*proto.SubscribeEvent)
			if !ok {
				return
			}
			facility := event.Event.GetFacility()
			eventType := event.Event.GetType()

			if facility != proto.EventSinkSinkInput {
				return
			}

			switch eventType {
			case proto.EventNew:
				// Non-blocking: buffer absorbs bursts; any overflow is caught by the next periodic refresh.
				select {
				case sf.newSinkInputCh <- event.Index:
				default:
				}
			case proto.EventChange:
				// Also non-blocking. Session map uses these to fight back when an app
				// (Firefox, Spotify, WirePlumber) overrides the volume we set.
				select {
				case sf.changedSinkInputCh <- event.Index:
				default:
				}
			}
		}
	}

	sf.logger.Debug("Created PA session finder instance")

	return sf, nil
}

func (sf *paSessionFinder) GetAllSessions() ([]Session, error) {
	sessions := []Session{}

	masterSink, err := sf.getMasterSinkSession()
	if err == nil {
		sessions = append(sessions, masterSink)
	} else {
		sf.logger.Warnw("Failed to get master audio sink session", "error", err)
	}

	masterSource, err := sf.getMasterSourceSession()
	if err == nil {
		sessions = append(sessions, masterSource)
	} else {
		sf.logger.Warnw("Failed to get master audio source session", "error", err)
	}

	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio sessions", "error", err)
		return nil, fmt.Errorf("enumerate audio sessions: %w", err)
	}

	return sessions, nil
}

// SubscribeToNewSessions returns a channel that receives the PulseAudio sink-input
// index each time a new audio stream is created.
func (sf *paSessionFinder) SubscribeToNewSessions() chan uint32 {
	return sf.newSinkInputCh
}

// SubscribeToSessionChanges returns a channel that receives the PulseAudio
// sink-input index each time an existing audio stream's properties change.
func (sf *paSessionFinder) SubscribeToSessionChanges() chan uint32 {
	return sf.changedSinkInputCh
}

func (sf *paSessionFinder) Release() error {
	if err := sf.conn.Close(); err != nil {
		sf.logger.Warnw("Failed to close PulseAudio connection", "error", err)
		return fmt.Errorf("close PulseAudio connection: %w", err)
	}

	sf.logger.Debug("Released PA session finder instance")

	return nil
}

func (sf *paSessionFinder) getMasterSinkSession() (Session, error) {
	request := proto.GetSinkInfo{
		SinkIndex: proto.Undefined,
	}
	reply := proto.GetSinkInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master sink info", "error", err)
		return nil, fmt.Errorf("get master sink info: %w", err)
	}

	sink := newMasterSession(sf.sessionLogger, sf.client, reply.SinkIndex, reply.Channels, true)
	return sink, nil
}

func (sf *paSessionFinder) getMasterSourceSession() (Session, error) {
	request := proto.GetSourceInfo{
		SourceIndex: proto.Undefined,
	}
	reply := proto.GetSourceInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master source info", "error", err)
		return nil, fmt.Errorf("get master source info: %w", err)
	}

	source := newMasterSession(sf.sessionLogger, sf.client, reply.SourceIndex, reply.Channels, false)
	return source, nil
}

func (sf *paSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	request := proto.GetSinkInputInfoList{}
	reply := proto.GetSinkInputInfoListReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get sink input list", "error", err)
		return fmt.Errorf("get sink input list: %w", err)
	}

	// Pre-fetch all PulseAudio clients so we can look up client-level properties
	// (e.g. application.process.binary) when they are absent from stream properties.
	// Since Spotify's 2025 update the binary name is only on the client object, not
	// on the individual sink-input, so without this the stream would be keyed as
	// "audio-src" and fall through to deej.unmapped instead of matching "spotify".
	clientProps := sf.fetchClientProps()

	for _, info := range reply {
		name, source := sf.resolveSessionName(info, clientProps)
		if name == "" {
			sf.logger.Warnw("Failed to get sink input's process name",
				"sinkInputIndex", info.SinkInputIndex)
			continue
		}

		sf.logger.Debugw("Resolved audio session name",
			"sinkInputIndex", info.SinkInputIndex,
			"name", name,
			"via", source)

		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name)
		*sessions = append(*sessions, newSession)
	}

	return nil
}

// fetchClientProps fetches all PulseAudio clients and returns a map from
// ClientIndex to their property list. On failure it logs a warning and returns
// an empty map so callers can degrade gracefully.
func (sf *paSessionFinder) fetchClientProps() map[uint32]proto.PropList {
	result := make(map[uint32]proto.PropList)

	clientReply := proto.GetClientInfoListReply{}
	if err := sf.client.Request(&proto.GetClientInfoList{}, &clientReply); err != nil {
		sf.logger.Warnw("Failed to fetch PA client list, skipping client-property fallback", "error", err)
		return result
	}

	for _, c := range clientReply {
		result[c.ClientIndex] = c.Properties
	}

	return result
}

// resolveSessionName picks the best human-readable name for a sink-input by
// checking multiple property sources in priority order:
//
//  1. stream.application.process.binary
//  2. stream.application.name
//  3. client.application.process.binary  (new Spotify sets name only here)
//  4. client.application.name
//  5. stream.node.name
//  6. stream.device.description
//
// Returns the resolved name and a short string describing which property was
// used (for debug logging). Returns ("", "") when no property is found.
func (sf *paSessionFinder) resolveSessionName(
	info *proto.GetSinkInputInfoReply,
	clientProps map[uint32]proto.PropList,
) (name, source string) {

	// helper: return the string value if the key exists and is non-empty.
	fromProps := func(props proto.PropList, key string) string {
		if v, ok := props[key]; ok {
			if s := v.String(); s != "" {
				return s
			}
		}
		return ""
	}

	candidates := []struct {
		props  proto.PropList
		key    string
		source string
	}{
		{info.Properties, "application.process.binary", "stream.application.process.binary"},
		{info.Properties, "application.name", "stream.application.name"},
		{clientProps[info.ClientIndex], "application.process.binary", "client.application.process.binary"},
		{clientProps[info.ClientIndex], "application.name", "client.application.name"},
		{info.Properties, "node.name", "stream.node.name"},
		{info.Properties, "device.description", "stream.device.description"},
	}

	for _, c := range candidates {
		if v := fromProps(c.props, c.key); v != "" {
			return v, c.source
		}
	}

	return "", ""
}
