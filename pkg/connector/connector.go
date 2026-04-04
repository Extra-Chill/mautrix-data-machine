package connector

import (
	"context"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

type DataMachineConnector struct {
	br             *bridgev2.Bridge
	Config         Config
	callbackServer *CallbackServer
}

var (
	_ bridgev2.NetworkConnector = (*DataMachineConnector)(nil)
)

func (dc *DataMachineConnector) Init(bridge *bridgev2.Bridge) {
	dc.br = bridge
	dc.callbackServer = NewCallbackServer(dc)
	bridge.Config.PersonalFilteringSpaces = false
}

func (dc *DataMachineConnector) Start(ctx context.Context) error {
	return dc.callbackServer.Start(ctx)
}

func (dc *DataMachineConnector) GetName() bridgev2.BridgeName {
	displayName := "Data Machine"
	if dc.Config.NetworkDisplayName != "" {
		displayName = dc.Config.NetworkDisplayName
	}

	return bridgev2.BridgeName{
		DisplayName:      displayName,
		NetworkURL:       "https://github.com/Extra-Chill/data-machine",
		NetworkID:        "datamachine",
		BeeperBridgeType: "sh-datamachine",
		DefaultPort:      29340,
	}
}

func (dc *DataMachineConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any {
			return &UserLoginMeta{}
		},
	}
}

func (dc *DataMachineConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		AggressiveUpdateInfo: false,
	}
}

func (dc *DataMachineConnector) GetBridgeInfoVersion() (info, caps int) {
	return 1, 1
}

func (dc *DataMachineConnector) LoadUserLogin(_ context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMeta)
	dmc := &DataMachineClient{
		Main:       dc,
		UserLogin:  login,
		siteURL:    meta.SiteURL,
		agentSlug:  meta.AgentSlug,
		agentToken: meta.AgentToken,
		httpClient: httpClientWithTimeout(dc.Config.RequestTimeout),
	}
	login.Client = dmc
	return nil
}

// UserLoginMeta stores per-login metadata in the bridge database.
type UserLoginMeta struct {
	SiteURL    string            `json:"site_url"`
	AgentSlug  string            `json:"agent_slug"`
	AgentName  string            `json:"agent_name,omitempty"`
	AgentToken string            `json:"agent_token"`
	SessionIDs map[string]string `json:"session_ids,omitempty"`
	// Tracks the last message timestamp per portal for session TTL rotation.
	LastMessageAt map[string]time.Time `json:"last_message_at,omitempty"`
	// Onboarding metadata from WordPress, cached at connect time.
	Onboarding *OnboardingData `json:"onboarding,omitempty"`
}

func (m *UserLoginMeta) RememberSessionID(portalKey, sessionID string) {
	if m.SessionIDs == nil {
		m.SessionIDs = make(map[string]string)
	}
	m.SessionIDs[portalKey] = sessionID
}

func (m *UserLoginMeta) SessionIDForPortal(portalKey string) string {
	if m.SessionIDs == nil {
		return ""
	}
	return m.SessionIDs[portalKey]
}

func (m *UserLoginMeta) HasSessionID(sessionID string) bool {
	for _, known := range m.SessionIDs {
		if known == sessionID {
			return true
		}
	}
	return false
}

// TouchPortal records the current time as the last message time for a portal.
func (m *UserLoginMeta) TouchPortal(portalKey string) {
	if m.LastMessageAt == nil {
		m.LastMessageAt = make(map[string]time.Time)
	}
	m.LastMessageAt[portalKey] = time.Now()
}

// IsSessionExpired checks if the session for a portal has been idle longer than ttl.
// Returns true if the session should be rotated.
func (m *UserLoginMeta) IsSessionExpired(portalKey string, ttl time.Duration) bool {
	if m.LastMessageAt == nil {
		return false
	}
	lastMsg, ok := m.LastMessageAt[portalKey]
	if !ok {
		return false
	}
	return time.Since(lastMsg) > ttl
}

// ClearSession removes the session ID and last-message timestamp for a portal,
// forcing the next message to create a fresh session.
func (m *UserLoginMeta) ClearSession(portalKey string) {
	delete(m.SessionIDs, portalKey)
	delete(m.LastMessageAt, portalKey)
}
