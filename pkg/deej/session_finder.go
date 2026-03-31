package deej

// SessionFinder represents an entity that can find all current audio sessions
// and notify when new ones appear or change.
type SessionFinder interface {
	GetAllSessions() ([]Session, error)

	// SubscribeToNewSessions returns a channel that receives the PulseAudio
	// sink-input index whenever a new audio session is created.
	SubscribeToNewSessions() chan uint32

	// SubscribeToSessionChanges returns a channel that receives the PulseAudio
	// sink-input index whenever an existing audio session changes (e.g. an app
	// or WirePlumber overrides the stream volume).
	SubscribeToSessionChanges() chan uint32

	Release() error
}
