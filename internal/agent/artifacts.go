package agent

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// artifactStore holds screenshots produced during a run so the UI can display
// them live.
//
// In memory and bounded on purpose. The locked design puts these in R2 so a
// failed verification can be examined long afterwards, and that is still the
// right destination — but object storage is not wired up yet, and writing
// artifact rows pointing at objects that do not exist would be worse than
// holding them here honestly. They are lost on restart, which is acceptable
// while their only consumer is the live timeline.
type artifactStore struct {
	mu    sync.RWMutex
	items map[string]artifact
	order []string
	limit int
}

type artifact struct {
	Data        []byte
	ContentType string
	RunID       int64
}

func newArtifactStore(limit int) *artifactStore {
	if limit <= 0 {
		limit = 40
	}
	return &artifactStore{items: map[string]artifact{}, limit: limit}
}

// put stores an artifact and returns its id.
func (s *artifactStore) put(runID int64, contentType string, data []byte) string {
	id := randomID()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.items[id] = artifact{Data: data, ContentType: contentType, RunID: runID}
	s.order = append(s.order, id)

	// Screenshots are a few hundred KB each; without a cap a long verification
	// would hold tens of megabytes on a 512 MB machine.
	for len(s.order) > s.limit {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.items, oldest)
	}

	return id
}

// get returns an artifact by id.
func (s *artifactStore) get(id string) (artifact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.items[id]
	return a, ok
}

func randomID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "artifact"
	}
	return hex.EncodeToString(buf)
}

func contentTypeFor(format string) string {
	switch format {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}
