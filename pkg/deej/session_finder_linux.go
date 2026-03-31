package deej

import (
	"fmt"
	"net"

	"github.com/jfreymuth/pulse/proto"
	"go.uber.org/zap"
)

// PulseAudio subscription constants (from pulseaudio/pulseaudio.h)
const (
	subscriptionMaskSinkInput          uint32 = 0x0004
	subscriptionEventFacilityMask      uint32 = 0x000F
	subscriptionEventTypeMask          uint32 = 0x0030
	subscriptionEventFacilitySinkInput uint32 = 0x0002
	subscriptionEventTypeNew           uint32 = 0x0000
	subscriptionEventTypeChange        uint32 = 0x0010
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
	if err := client.Request(&proto.Subscribe{Mask: subscriptionMaskSinkInput}, nil); err != nil {
		sf.logger.Warnw("Failed to subscribe to PulseAudio sink-input events, volume-spike prevention disabled", "error", err)
	} else {
		client.Callback = func(msg interface{}) {
			event, ok := msg.(*proto.SubscribeEvent)
			if !ok {
				return
			}
			facility := event.Event & subscriptionEventFacilityMask
			eventType := event.Event & subscriptionEventTypeMask

			if facility != subscriptionEventFacilitySinkInput {
				return
			}

			switch eventType {
			case subscriptionEventTypeNew:
				// Non-blocking: buffer absorbs bursts; any overflow is caught by the next periodic refresh.
				select {
				case sf.newSinkInputCh <- event.Index:
				default:
				}
			case subscriptionEventTypeChange:
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

	for _, info := range reply {
		name, ok := info.Properties["application.process.binary"]

		// try fallback properties for pipewire-pulse
		if !ok {
			name, ok = info.Properties["node.name"]
			if !ok {
				name, ok = info.Properties["device.description"]
				if !ok {
					sf.logger.Warnw("Failed to get sink input's process name",
						"sinkInputIndex", info.SinkInputIndex)
					continue
				}
			}
		}

		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name.String())
		*sessions = append(*sessions, newSession)
	}

	return nil
}
