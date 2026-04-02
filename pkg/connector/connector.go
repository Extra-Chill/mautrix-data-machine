package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

type DataMachineConnector struct {
	br     *bridgev2.Bridge
	Config Config
}

var (
	_ bridgev2.NetworkConnector = (*DataMachineConnector)(nil)
)

func (dc *DataMachineConnector) Init(bridge *bridgev2.Bridge) {
	dc.br = bridge
	bridge.Config.PersonalFilteringSpaces = false
}

func (dc *DataMachineConnector) Start(ctx context.Context) error {
	// No global startup actions needed; per-login connections start in LoadUserLogin/Connect.
	return nil
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
	SiteURL    string `json:"site_url"`
	AgentSlug  string `json:"agent_slug"`
	AgentToken string `json:"agent_token"`
}
