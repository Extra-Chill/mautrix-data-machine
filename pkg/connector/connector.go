package connector

import (
	"context"

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
	return bridgev2.BridgeName{
		DisplayName:      "Data Machine",
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
	SiteURL     string            `json:"site_url"`
	AgentSlug   string            `json:"agent_slug"`
	AgentName   string            `json:"agent_name,omitempty"`
	AgentToken  string            `json:"agent_token"`
	SessionIDs  map[string]string `json:"session_ids,omitempty"`
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
