package connector

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

const LoginFlowIDToken = "token"
const LoginStepIDSiteToken = "fi.mau.datamachine.login.enter_site_token"

func (dc *DataMachineConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{
		{
			Name:        "Site URL + Agent Token",
			Description: "Log in to a Data Machine agent using your WordPress site URL and agent token",
			ID:          LoginFlowIDToken,
		},
	}
}

func (dc *DataMachineConnector) CreateLogin(_ context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowIDToken {
		return nil, fmt.Errorf("unsupported login flow: %s", flowID)
	}
	return &DataMachineLogin{User: user, Main: dc}, nil
}

const LoginStepIDComplete = "fi.mau.datamachine.login.complete"

type DataMachineLogin struct {
	User *bridgev2.User
	Main *DataMachineConnector
}

var _ bridgev2.LoginProcessUserInput = (*DataMachineLogin)(nil)

func (d *DataMachineLogin) Start(_ context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepIDSiteToken,
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        bridgev2.LoginInputFieldTypeURL,
					ID:          "site_url",
					Name:        "Site URL",
					Description: "The URL of your WordPress site (e.g. https://example.com)",
					Pattern:     "^https?://.+$",
				},
				{
					Type:        bridgev2.LoginInputFieldTypeToken,
					ID:          "agent_token",
					Name:        "Agent Token",
					Description: "Your Data Machine agent token (starts with datamachine_)",
					Pattern:     "^datamachine_.+$",
				},
			},
		},
	}, nil
}

func (d *DataMachineLogin) Cancel() {}

func (d *DataMachineLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	siteURL := input["site_url"]
	agentToken := input["agent_token"]

	if siteURL == "" || agentToken == "" {
		return nil, fmt.Errorf("site URL and agent token are required")
	}

	// Create a temporary client to validate the credentials.
	tempClient := &DataMachineClient{
		Main:       d.Main,
		siteURL:    siteURL,
		agentToken: agentToken,
		httpClient: httpClientWithTimeout(d.Main.Config.RequestTimeout),
	}

	ident, err := tempClient.GetIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to validate credentials: %w", err)
	}

	// Build the UserLoginID from site host + agent slug.
	loginID := networkid.UserLoginID(fmt.Sprintf("%s:%s", hostFromURL(siteURL), ident.AgentSlug))

	ul, err := d.User.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: ident.AgentName,
		Metadata: &UserLoginMeta{
			SiteURL:    siteURL,
			AgentSlug:  ident.AgentSlug,
			AgentToken: agentToken,
		},
	}, &bridgev2.NewLoginParams{
		DontReuseExisting: false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create login: %w", err)
	}

	// Start the connection in the background.
	dmc := ul.Client.(*DataMachineClient)
	go dmc.Connect(ul.Log.WithContext(context.Background()))

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepIDComplete,
		Instructions: fmt.Sprintf("Successfully connected to %s as agent %s", siteURL, ident.AgentName),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}
