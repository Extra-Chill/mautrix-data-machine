package wordpress

import (
	"sync"
	"time"
)

// SessionStore manages per-portal session IDs and last-message timestamps
// for session TTL rotation. It is safe for concurrent use.
type SessionStore struct {
	mu            sync.RWMutex
	SessionIDs    map[string]string    `json:"session_ids,omitempty"`
	LastMessageAt map[string]time.Time `json:"last_message_at,omitempty"`
}

// NewSessionStore creates a SessionStore with initialized maps.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		SessionIDs:    make(map[string]string),
		LastMessageAt: make(map[string]time.Time),
	}
}

// RememberSessionID stores a session ID for the given portal key.
func (s *SessionStore) RememberSessionID(portalKey, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SessionIDs == nil {
		s.SessionIDs = make(map[string]string)
	}
	s.SessionIDs[portalKey] = sessionID
}

// SessionIDForPortal returns the stored session ID for a portal, or empty string.
func (s *SessionStore) SessionIDForPortal(portalKey string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.SessionIDs == nil {
		return ""
	}
	return s.SessionIDs[portalKey]
}

// HasSessionID checks if any portal is using the given session ID.
func (s *SessionStore) HasSessionID(sessionID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, known := range s.SessionIDs {
		if known == sessionID {
			return true
		}
	}
	return false
}

// AllSessionIDs returns all stored session IDs (values, not keys).
func (s *SessionStore) AllSessionIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for _, v := range s.SessionIDs {
		ids = append(ids, v)
	}
	return ids
}

// TouchPortal records the current time as the last message time for a portal.
func (s *SessionStore) TouchPortal(portalKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LastMessageAt == nil {
		s.LastMessageAt = make(map[string]time.Time)
	}
	s.LastMessageAt[portalKey] = time.Now()
}

// IsSessionExpired checks if the session for a portal has been idle longer than ttl.
func (s *SessionStore) IsSessionExpired(portalKey string, ttl time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.LastMessageAt == nil {
		return false
	}
	lastMsg, ok := s.LastMessageAt[portalKey]
	if !ok {
		return false
	}
	return time.Since(lastMsg) > ttl
}

// ClearSession removes the session ID and last-message timestamp for a portal.
func (s *SessionStore) ClearSession(portalKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.SessionIDs, portalKey)
	delete(s.LastMessageAt, portalKey)
}
