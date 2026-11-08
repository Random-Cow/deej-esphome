package deej

import (
	"sync"
	"testing"
)

type fakeSession struct{ key string }

func (f *fakeSession) GetVolume() float32      { return 0 }
func (f *fakeSession) SetVolume(float32) error { return nil }
func (f *fakeSession) Key() string             { return f.key }
func (f *fakeSession) Release()                {}

func TestGetMatchingKeyWithGlobMetacharacters(t *testing.T) {
	m := &sessionMap{m: map[string][]Session{}, lock: &sync.Mutex{}}
	key := "pipewire alsa [some-app]"
	m.add(&fakeSession{key: key})
	m.add(&fakeSession{key: "firefox"})

	if got, ok := m.getMatching(key); !ok || len(got) != 1 {
		t.Fatalf("exact key with brackets: got %d sessions, ok=%v", len(got), ok)
	}
	if got, ok := m.getMatching("fire*"); !ok || len(got) != 1 {
		t.Fatalf("glob: got %d sessions, ok=%v", len(got), ok)
	}
	if _, ok := m.getMatching("spotify"); ok {
		t.Fatal("unexpected match for absent key")
	}
}
