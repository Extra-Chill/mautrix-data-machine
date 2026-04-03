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
	"time"

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
		Description: "Sign in to your WordPress site and authorize the agent in your browser",
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

	siteURL    string
	pkce       *PKCELoginState
	onboarding *OnboardingData
}

var _ bridgev2.LoginProcessUserInput = (*DataMachineLogin)(nil)
var _ bridgev2.LoginProcessDisplayAndWait = (*DataMachineLogin)(nil)

func (d *DataMachineLogin) Start(_ context.Context) (*bridgev2.LoginStep, error) {
	defaultSiteURL := d.Main.Config.DefaultSiteURL
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepIDSiteURL,
		Instructions: "Enter the WordPress site URL where the agent is available.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:         bridgev2.LoginInputFieldTypeURL,
				ID:           "site_url",
				Name:         "Site URL",
				Description:  "The WordPress site URL (e.g., https://studio.extrachill.com)",
				DefaultValue: defaultSiteURL,
				Pattern:      "^https?://.+$",
			}},
		},
	}, nil
}

func (d *DataMachineLogin) Cancel() {}

func (d *DataMachineLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
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

	// Fetch onboarding metadata before showing the QR code.
	// This populates login instructions and agent display info from WordPress.
	onboarding, err := fetchOnboarding(ctx, siteURL, d.Main.Config.AgentSlug, d.Main.Config.RequestTimeout)
	if err == nil && onboarding != nil && onboarding.Data != nil {
		d.onboarding = onboarding.Data
	}

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

	// Use login instructions from onboarding metadata if available.
	instructions := "Sign in and authorize the agent. You can scan the QR code or open the link in a browser."
	if d.onboarding != nil && d.onboarding.LoginInstructions != "" {
		instructions = d.onboarding.LoginInstructions
	}

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeDisplayAndWait,
		StepID:       LoginStepIDAuthorize,
		Instructions: instructions,
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

		displayName := tokenResp.AgentName
		if d.onboarding != nil && d.onboarding.DisplayName != "" {
			displayName = d.onboarding.DisplayName
		}

		loginID := networkid.UserLoginID(fmt.Sprintf("%s:%s", hostFromURL(d.siteURL), tokenResp.AgentSlug))
		meta := &UserLoginMeta{
			SiteURL:    d.siteURL,
			AgentSlug:  tokenResp.AgentSlug,
			AgentName:  displayName,
			AgentToken: tokenResp.AccessToken,
			SessionIDs: map[string]string{},
			Onboarding: d.onboarding,
		}

		ul, err := d.User.NewLogin(ctx, &database.UserLogin{
			ID:         loginID,
			RemoteName: displayName,
			Metadata:   meta,
		}, &bridgev2.NewLoginParams{DontReuseExisting: false})
		if err != nil {
			return nil, fmt.Errorf("failed to create login: %w", err)
		}

		go ul.Client.(*DataMachineClient).Connect(ul.Log.WithContext(context.Background()))

		completeMessage := fmt.Sprintf("Successfully connected to %s as %s", d.siteURL, displayName)
		if d.onboarding != nil && d.onboarding.WelcomeMessage != "" {
			completeMessage = d.onboarding.WelcomeMessage
		}

		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeComplete,
			StepID:       LoginStepIDComplete,
			Instructions: completeMessage,
			CompleteParams: &bridgev2.LoginCompleteParams{
				UserLoginID: ul.ID,
				UserLogin:   ul,
			},
		}, nil
	}
}

func (d *DataMachineLogin) exchangeAuthorizationCode(ctx context.Context, code string) (*TokenExchangeResponse, error) {
	payload := map[string]string{
		"code":          code,
		"code_verifier": d.pkce.CodeVerifier,
		"redirect_uri":  d.pkce.CallbackURL,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(d.pkce.SiteURL, "/") + "/wp-json/datamachine/v1/bridge/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := httpClientWithTimeout(d.Main.Config.RequestTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp TokenExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// fetchOnboarding retrieves onboarding metadata from a WordPress site.
// Used during the login flow before a DataMachineClient exists.
func fetchOnboarding(ctx context.Context, siteURL, agentSlug string, timeout time.Duration) (*OnboardingResponse, error) {
	endpoint := strings.TrimRight(siteURL, "/") + "/wp-json/datamachine/v1/bridge/onboarding"
	if agentSlug != "" {
		endpoint += "?agent_slug=" + agentSlug
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	client := httpClientWithTimeout(timeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onboarding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("onboarding returned %d: %s", resp.StatusCode, string(body))
	}

	var onboarding OnboardingResponse
	if err := json.NewDecoder(resp.Body).Decode(&onboarding); err != nil {
		return nil, fmt.Errorf("failed to decode onboarding: %w", err)
	}
	return &onboarding, nil
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
