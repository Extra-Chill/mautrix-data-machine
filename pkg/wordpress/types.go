package wordpress

import "time"

// SendResponse is the /datamachine/v1/bridge/send API response.
type SendResponse struct {
	Success   bool   `json:"success"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Response  string `json:"response"`
	Completed bool   `json:"completed"`
}

// Attachment mirrors one entry in the /bridge/send `attachments` array,
// which in turn mirrors the /datamachine/v1/chat schema consumed by
// Data Machine's ChatOrchestrator.
//
// Each attachment must carry at least one of URL or MediaID. The
// Extra Chill PHP side drops items missing both.
type Attachment struct {
	URL      string `json:"url,omitempty"`
	MediaID  int64  `json:"media_id,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// BridgeContext is optional per-room metadata forwarded to /bridge/send
// so the Data Machine agent's bridge-mode guidance can adapt its tone
// and tool suggestions to the upstream app (iMessage vs WhatsApp vs
// Signal etc.) and conversation shape (dm/group/channel).
type BridgeContext struct {
	App      string `json:"bridge_app,omitempty"`
	Room     string `json:"bridge_room,omitempty"`
	RoomKind string `json:"bridge_room_kind,omitempty"`
}

// MediaUploadResponse is a subset of the WP REST /wp/v2/media response.
// We only need the attachment ID, the canonical public URL, and the
// server-computed mime type — everything else (title, rendered HTML,
// EXIF metadata etc.) is not required for forwarding to /bridge/send.
type MediaUploadResponse struct {
	ID        int64  `json:"id"`
	SourceURL string `json:"source_url"`
	MimeType  string `json:"mime_type"`
}

// PendingEnvelope wraps the /datamachine/v1/bridge/pending API response.
type PendingEnvelope struct {
	Success  bool             `json:"success"`
	Messages []PendingMessage `json:"messages"`
	Count    int              `json:"count"`
}

// PendingMessage is a single queued message from WordPress.
type PendingMessage struct {
	QueueID   string                 `json:"queue_id"`
	SessionID string                 `json:"session_id"`
	AgentID   int                    `json:"agent_id"`
	UserID    int                    `json:"user_id"`
	Role      string                 `json:"role"`
	Content   string                 `json:"content"`
	Completed bool                   `json:"completed"`
	Timestamp string                 `json:"timestamp"`
	CreatedAt string                 `json:"created_at"`
	Metadata  map[string]interface{} `json:"metadata"`
	AgentSlug string                 `json:"agent_slug,omitempty"`
	SiteURL   string                 `json:"site_url,omitempty"`
}

// Time returns the parsed timestamp from either field.
func (pm PendingMessage) Time() time.Time {
	if pm.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339, pm.Timestamp); err == nil {
			return parsed
		}
	}
	if pm.CreatedAt != "" {
		if parsed, err := time.Parse("2006-01-02 15:04:05", pm.CreatedAt); err == nil {
			return parsed
		}
	}
	return time.Now()
}

// IdentityResponse is the /datamachine/v1/bridge/identity API response.
type IdentityResponse struct {
	Success   bool          `json:"success"`
	Data      *IdentityData `json:"data,omitempty"`
	AgentID   int           `json:"agent_id,omitempty"`
	AgentSlug string        `json:"agent_slug,omitempty"`
	AgentName string        `json:"agent_name,omitempty"`
	Status    string        `json:"status,omitempty"`
	SiteURL   string        `json:"site_url,omitempty"`
	SiteName  string        `json:"site_name,omitempty"`
	SiteHost  string        `json:"site_host,omitempty"`
}

// IdentityData holds the agent identity fields.
type IdentityData struct {
	AgentID   int    `json:"agent_id"`
	AgentSlug string `json:"agent_slug"`
	AgentName string `json:"agent_name"`
	Status    string `json:"status"`
	SiteURL   string `json:"site_url"`
	SiteName  string `json:"site_name"`
	SiteHost  string `json:"site_host"`
}

// TokenExchangeResponse is the /datamachine/v1/bridge/token API response.
type TokenExchangeResponse struct {
	Success     bool   `json:"success"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	TokenID     int    `json:"token_id"`
	TokenLabel  string `json:"token_label"`
	AgentID     int    `json:"agent_id"`
	AgentSlug   string `json:"agent_slug"`
	AgentName   string `json:"agent_name"`
	SiteURL     string `json:"site_url"`
	SiteHost    string `json:"site_host"`
}

// OnboardingResponse is the /datamachine/v1/bridge/onboarding API response.
type OnboardingResponse struct {
	Success bool            `json:"success"`
	Data    *OnboardingData `json:"data,omitempty"`
}

// OnboardingData holds the onboarding metadata for a bridge client's first-run UX.
type OnboardingData struct {
	SiteURL           string            `json:"site_url"`
	SiteName          string            `json:"site_name"`
	SiteHost          string            `json:"site_host"`
	AuthorizeURL      string            `json:"authorize_url"`
	Agent             *OnboardingAgent  `json:"agent,omitempty"`
	DisplayName       string            `json:"display_name"`
	Description       string            `json:"description"`
	AvatarURL         string            `json:"avatar_url"`
	WelcomeMessage    string            `json:"welcome_message"`
	LoginLabel        string            `json:"login_label"`
	LoginInstructions string            `json:"login_instructions"`
	Capabilities      map[string]string `json:"capabilities"`
	RoomName          string            `json:"room_name"`
	RoomTopic         string            `json:"room_topic"`
}

// OnboardingAgent holds agent identity from the onboarding endpoint.
type OnboardingAgent struct {
	AgentID   int    `json:"agent_id"`
	AgentSlug string `json:"agent_slug"`
	AgentName string `json:"agent_name"`
	Status    string `json:"status"`
}
