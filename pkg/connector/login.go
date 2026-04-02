package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

const LoginFlowIDBrowser = "browser"
const LoginStepIDSiteURL = "fi.mau.datamachine.login.enter_site_url"
const LoginStepIDAuthorize = "fi.mau.datamachine.login.authorize"
const LoginStepIDComplete = "fi.mau.datamachine.login.complete"

func (dc *DataMachineConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Browser authorization",
		Description: "Sign in to Extra Chill and authorize Roadie in your browser",
		ID:          LoginFlowIDBrowser,
	}}
}

func (dc *DataMachineConnector) CreateLogin(_ context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowIDBrowser {
		return nil, fmt.Errorf("unsupported login flow: %s", flowID)
	}
	return &DataMachineLogin{User: user, Main: dc}, nil
}

type DataMachineLogin struct {
	User *bridgev2.User
	Main *DataMachineConnector

	siteURL string
	pkce    *PKCELoginState
}

var _ bridgev2.LoginProcessUserInput = (*DataMachineLogin)(nil)
var _ bridgev2.LoginProcessDisplayAndWait = (*DataMachineLogin)(nil)

func (d *DataMachineLogin) Start(_ context.Context) (*bridgev2.LoginStep, error) {
	defaultSiteURL := d.Main.Config.DefaultSiteURL
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepIDSiteURL,
		Instructions: "Enter the Extra Chill site URL where Roadie is available.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:         bridgev2.LoginInputFieldTypeURL,
				ID:           "site_url",
				Name:         "Site URL",
				Description:  "For Extra Chill this is usually https://studio.extrachill.com",
				DefaultValue: defaultSiteURL,
				Pattern:      "^https?://.+$",
			}},
		},
	}, nil
}

func (d *DataMachineLogin) Cancel() {}

func (d *DataMachineLogin) SubmitUserInput(_ context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	siteURL := normalizeBaseURL(input["site_url"])
	if siteURL == "" {
		return nil, fmt.Errorf("site URL is required")
	}

	callbackURL, err := resolveCallbackURL(d.Main.Config.CallbackURL)
	if err != nil {
		return nil, err
	}

	verifier, err := generateRandomString(32)
	if err != nil {
		return nil, err
	}
	state, err := generateRandomString(24)
	if err != nil {
		return nil, err
	}

	d.siteURL = siteURL
	d.pkce = &PKCELoginState{
		SiteURL:      siteURL,
		CallbackURL:  callbackURL,
		CodeVerifier: verifier,
		State:        state,
		ResultCh:     make(chan PKCECallbackResult, 1),
	}
	d.Main.callbackServer.RegisterLogin(state, d.pkce)

	authorizeURL, err := d.buildAuthorizeURL(siteURL, callbackURL, verifier, state)
	if err != nil {
		return nil, err
	}

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeDisplayAndWait,
		StepID:       LoginStepIDAuthorize,
		Instructions: "Sign in to Extra Chill and click **Authorize Roadie**. You can scan the QR code or open the link in a browser.",
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
			Type: bridgev2.LoginDisplayTypeQR,
			Data: authorizeURL,
		},
	}, nil
}

func (d *DataMachineLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	if d.pkce == nil {
		return nil, fmt.Errorf("login state not initialized")
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-d.pkce.ResultCh:
		if result.Error != "" {
			return nil, fmt.Errorf("authorization failed: %s", result.Error)
		}
		if result.State != d.pkce.State {
			return nil, fmt.Errorf("authorization state mismatch")
		}

		tokenResp, err := d.exchangeAuthorizationCode(ctx, result.Code)
		if err != nil {
			return nil, err
		}

		loginID := networkid.UserLoginID(fmt.Sprintf("%s:%s", hostFromURL(d.siteURL), tokenResp.AgentSlug))
		meta := &UserLoginMeta{
			SiteURL:    d.siteURL,
			AgentSlug:  tokenResp.AgentSlug,
			AgentName:  tokenResp.AgentName,
			AgentToken: tokenResp.AccessToken,
			SessionIDs: map[string]string{},
		}

		ul, err := d.User.NewLogin(ctx, &database.UserLogin{
			ID:         loginID,
			RemoteName: tokenResp.AgentName,
			Metadata:   meta,
		}, &bridgev2.NewLoginParams{DontReuseExisting: false})
		if err != nil {
			return nil, fmt.Errorf("failed to create login: %w", err)
		}

		go ul.Client.(*DataMachineClient).Connect(ul.Log.WithContext(context.Background()))

		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeComplete,
			StepID:       LoginStepIDComplete,
			Instructions: fmt.Sprintf("Successfully connected to %s as %s", d.siteURL, tokenResp.AgentName),
			CompleteParams: &bridgev2.LoginCompleteParams{
				UserLoginID: ul.ID,
				UserLogin:   ul,
			},
		}, nil
	}
}

func (d *DataMachineLogin) buildAuthorizeURL(siteURL, callbackURL, verifier, state string) (string, error) {
	base := strings.TrimRight(siteURL, "/") + "/wp-json/datamachine/v1/agent/authorize"
	params := url.Values{}
	params.Set("agent_slug", d.Main.Config.AgentSlug)
	params.Set("redirect_uri", callbackURL)
	params.Set("label", fmt.Sprintf("beeper-%s", d.Main.Config.AgentSlug))
	params.Set("code_challenge", pkceChallenge(verifier))
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	return base + "?" + params.Encode(), nil
}
