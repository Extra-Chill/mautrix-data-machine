package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

func init() {
	status.BridgeStateHumanErrors.Update(status.BridgeStateErrorMap{
		"datamachine-not-logged-in":   "Please sign in and authorize the agent",
		"datamachine-invalid-auth":    "Your login is no longer valid. Please log in again",
		"datamachine-missing-session": "This conversation is not linked to a Data Machine session yet",
	})
}

type DataMachineClient struct {
	Main      *DataMachineConnector
	UserLogin *bridgev2.UserLogin

	siteURL    string
	agentSlug  string
	agentToken string
	httpClient *http.Client

	pollCancel context.CancelFunc
	stopOnce   sync.Once
}

var _ bridgev2.NetworkAPI = (*DataMachineClient)(nil)

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

type IdentityData struct {
	AgentID   int    `json:"agent_id"`
	AgentSlug string `json:"agent_slug"`
	AgentName string `json:"agent_name"`
	Status    string `json:"status"`
	SiteURL   string `json:"site_url"`
	SiteName  string `json:"site_name"`
	SiteHost  string `json:"site_host"`
}

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

// SendResponse is the /datamachine/v1/bridge/send API response.
type SendResponse struct {
	Success   bool   `json:"success"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Response  string `json:"response"`
	Completed bool   `json:"completed"`
}

type PendingEnvelope struct {
	Success  bool             `json:"success"`
	Messages []PendingMessage `json:"messages"`
	Count    int              `json:"count"`
}

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

func (dmc *DataMachineClient) Connect(ctx context.Context) {
	if dmc.agentToken == "" || dmc.siteURL == "" {
		dmc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "datamachine-not-logged-in"})
		return
	}

	log := zerolog.Ctx(ctx)

	ident, err := dmc.GetIdentity(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to validate agent token")
		dmc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "datamachine-invalid-auth", Message: err.Error()})
		return
	}

	meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
	if ident.Data != nil {
		dmc.agentSlug = ident.Data.AgentSlug
		meta.AgentSlug = ident.Data.AgentSlug
		meta.AgentName = ident.Data.AgentName
	}

	// Fetch onboarding metadata to configure room display and welcome message.
	onboarding, err := dmc.GetOnboarding(ctx, dmc.agentSlug)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch onboarding metadata (non-fatal)")
	} else if onboarding != nil && onboarding.Data != nil {
		meta.Onboarding = onboarding.Data
		if onboarding.Data.DisplayName != "" {
			meta.AgentName = onboarding.Data.DisplayName
		}
	}

	if dmc.Main.Config.CallbackURL != "" {
		if err := dmc.RegisterBridge(ctx); err != nil {
			log.Warn().Err(err).Msg("Failed to register bridge webhook callback")
		}
	}

	// Create or resolve the portal room for this agent so the user has
	// a place to chat. Without this, messages go to the management room.
	portalID := networkid.PortalID(dmc.agentSlug)
	portalKey := networkid.PortalKey{
		ID:       portalID,
		Receiver: dmc.UserLogin.ID,
	}
	portal, err := dmc.Main.br.GetPortalByKey(ctx, portalKey)
	if err != nil {
		log.Err(err).Msg("Failed to get/create portal")
	} else if portal != nil {
		// Ensure the room exists on Matrix.
		err = portal.CreateMatrixRoom(ctx, dmc.UserLogin, nil)
		if err != nil {
			log.Err(err).Msg("Failed to create Matrix room for portal")
		}
	}

	go dmc.startPolling()
	dmc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
}

func (dmc *DataMachineClient) Disconnect() {
	dmc.stopOnce.Do(func() {
		if dmc.pollCancel != nil {
			dmc.pollCancel()
		}
	})
}

func (dmc *DataMachineClient) IsLoggedIn() bool {
	return dmc.agentToken != "" && dmc.siteURL != ""
}

func (dmc *DataMachineClient) LogoutRemote(ctx context.Context) {
	dmc.Disconnect()
	dmc.agentToken = ""
	dmc.siteURL = ""
	meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
	meta.AgentToken = ""
	meta.SiteURL = ""
}

func (dmc *DataMachineClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return networkid.UserID(dmc.agentSlug+"@"+hostFromURL(dmc.siteURL)) == userID
}

func (dmc *DataMachineClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	return &event.RoomFeatures{
		ID: "fi.mau.datamachine",
	}
}

func (dmc *DataMachineClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
	dmType := database.RoomTypeDM
	info := &bridgev2.ChatInfo{
		Type: &dmType,
	}

	// Use onboarding metadata for room name and topic.
	if meta.Onboarding != nil && meta.Onboarding.RoomName != "" {
		info.Name = ptrStr(meta.Onboarding.RoomName)
	} else if meta.AgentName != "" {
		info.Name = ptrStr(meta.AgentName)
	} else if portal.Name != "" {
		info.Name = ptrStr(portal.Name)
	} else {
		info.Name = ptrStr(dmc.agentSlug)
	}

	if meta.Onboarding != nil && meta.Onboarding.RoomTopic != "" {
		info.Topic = ptrStr(meta.Onboarding.RoomTopic)
	}

	// Members: the ghost (agent) and the user.
	agentGhostID := networkid.UserID(dmc.agentSlug)
	info.Members = &bridgev2.ChatMemberList{
		IsFull: true,
		Members: []bridgev2.ChatMember{
			{
				EventSender: bridgev2.EventSender{
					SenderLogin: dmc.UserLogin.ID,
					Sender:      agentGhostID,
				},
			},
		},
	}

	return info, nil
}

func (dmc *DataMachineClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
	name := dmc.agentSlug

	if meta.Onboarding != nil && meta.Onboarding.DisplayName != "" {
		name = meta.Onboarding.DisplayName
	} else if meta.AgentName != "" {
		name = meta.AgentName
	} else if ghost.Name != "" {
		name = ghost.Name
	}

	userInfo := &bridgev2.UserInfo{Name: ptrStr(name)}

	// Set avatar from onboarding metadata.
	if meta.Onboarding != nil && meta.Onboarding.AvatarURL != "" {
		avatarURL := meta.Onboarding.AvatarURL
		userInfo.Avatar = &bridgev2.Avatar{
			ID: networkid.AvatarID(avatarURL),
			Get: func(ctx context.Context) ([]byte, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, avatarURL, nil)
				if err != nil {
					return nil, err
				}
				resp, err := dmc.httpClient.Do(req)
				if err != nil {
					return nil, err
				}
				defer resp.Body.Close()
				return io.ReadAll(resp.Body)
			},
		}
	}

	return userInfo, nil
}

func (dmc *DataMachineClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if !dmc.IsLoggedIn() {
		return nil, bridgev2.ErrNotLoggedIn
	}

	// Prefer plain text body for the chat API (HTML can confuse the agent).
	text := msg.Content.Body

	sessionID, err := dmc.ensurePortalSessionID(ctx, msg.Portal)
	if err != nil {
		return nil, err
	}

	sendResp, err := dmc.SendMessage(ctx, text, sessionID)
	if err != nil {
		return nil, err
	}

	// Update stored session ID if the server returned a new one.
	if sendResp.SessionID != "" && sendResp.SessionID != sessionID {
		meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
		portalKey := string(msg.Portal.PortalKey.ID) + "|" + string(msg.Portal.PortalKey.Receiver)
		meta.RememberSessionID(portalKey, sendResp.SessionID)
		if err := dmc.UserLogin.Save(ctx); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to save updated session ID")
		}
	}

	msgID := networkid.MessageID(sendResp.MessageID)
	if msgID == "" {
		msgID = networkid.MessageID(msg.Event.ID)
	}

	return &bridgev2.MatrixMessageResponse{DB: &database.Message{ID: msgID}}, nil
}

// SendMessage sends a user message via the bridge /send endpoint.
// This uses the dedicated bridge inbound endpoint instead of the raw /chat API.
func (dmc *DataMachineClient) SendMessage(ctx context.Context, message, sessionID string) (*SendResponse, error) {
	payload := map[string]string{
		"message": message,
	}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal send payload: %w", err)
	}

	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/datamachine/v1/bridge/send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dmc.agentToken)

	resp, err := dmc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("send returned %d: %s", resp.StatusCode, string(respBody))
	}

	var sendResp SendResponse
	if err := json.NewDecoder(resp.Body).Decode(&sendResp); err != nil {
		return nil, fmt.Errorf("failed to decode send response: %w", err)
	}

	return &sendResp, nil
}

func (dmc *DataMachineClient) GetIdentity(ctx context.Context) (*IdentityResponse, error) {
	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/datamachine/v1/bridge/identity"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+dmc.agentToken)

	resp, err := dmc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("identity check returned %d: %s", resp.StatusCode, string(body))
	}

	var ident IdentityResponse
	if err := json.NewDecoder(resp.Body).Decode(&ident); err != nil {
		return nil, fmt.Errorf("failed to decode identity response: %w", err)
	}
	if ident.Data == nil {
		ident.Data = &IdentityData{
			AgentID:   ident.AgentID,
			AgentSlug: ident.AgentSlug,
			AgentName: ident.AgentName,
			Status:    ident.Status,
			SiteURL:   ident.SiteURL,
			SiteName:  ident.SiteName,
			SiteHost:  ident.SiteHost,
		}
	}
	return &ident, nil
}

// GetOnboarding fetches the onboarding metadata from the WordPress site.
// This is called during connect and during the login flow to populate
// display information, welcome messages, and room configuration.
// The endpoint is unauthenticated — it works before login too.
func (dmc *DataMachineClient) GetOnboarding(ctx context.Context, agentSlug string) (*OnboardingResponse, error) {
	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/datamachine/v1/bridge/onboarding"
	if agentSlug != "" {
		url += "?agent_slug=" + agentSlug
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Include auth if available (not required, but may yield richer metadata).
	if dmc.agentToken != "" {
		req.Header.Set("Authorization", "Bearer "+dmc.agentToken)
	}

	resp, err := dmc.httpClient.Do(req)
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
		return nil, fmt.Errorf("failed to decode onboarding response: %w", err)
	}
	return &onboarding, nil
}

func (dmc *DataMachineClient) RegisterBridge(ctx context.Context) error {
	callbackURL, err := webhookURLFromCallback(dmc.Main.Config.CallbackURL)
	if err != nil {
		return err
	}
	payload := map[string]string{
		"callback_url": callbackURL,
		"bridge_id":    "mautrix-datamachine",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/datamachine/v1/bridge/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dmc.agentToken)

	resp, err := dmc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (dmc *DataMachineClient) GetPendingMessages(ctx context.Context) ([]PendingMessage, error) {
	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/datamachine/v1/bridge/pending"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+dmc.agentToken)

	meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
	if ids := meta.SessionIDs; len(ids) > 0 {
		query := req.URL.Query()
		for _, sessionID := range ids {
			query.Add("session_ids", sessionID)
		}
		req.URL.RawQuery = query.Encode()
	}

	resp, err := dmc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pending request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pending returned %d: %s", resp.StatusCode, string(body))
	}

	var envelope PendingEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("failed to decode pending messages: %w", err)
	}
	for i := range envelope.Messages {
		if envelope.Messages[i].AgentSlug == "" {
			envelope.Messages[i].AgentSlug = dmc.agentSlug
		}
		if envelope.Messages[i].SiteURL == "" {
			envelope.Messages[i].SiteURL = dmc.siteURL
		}
	}
	return envelope.Messages, nil
}

func (dmc *DataMachineClient) AckPendingMessages(ctx context.Context, ids []string) error {
	payload := map[string][]string{"message_ids": ids}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/datamachine/v1/bridge/ack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dmc.agentToken)

	resp, err := dmc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ack request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ack returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (dmc *DataMachineClient) ensurePortalSessionID(ctx context.Context, portal *bridgev2.Portal) (string, error) {
	meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
	portalKey := string(portal.PortalKey.ID) + "|" + string(portal.PortalKey.Receiver)
	if portalKey == "" {
		portalKey = string(portal.ID)
	}

	if existing := meta.SessionIDForPortal(portalKey); existing != "" {
		return existing, nil
	}

	sessionID := fmt.Sprintf("mx:%s:%s", dmc.UserLogin.ID, portalKey)
	meta.RememberSessionID(portalKey, sessionID)
	if err := dmc.UserLogin.Save(ctx); err != nil {
		return "", fmt.Errorf("failed to save portal session mapping: %w", err)
	}
	return sessionID, nil
}

func (dmc *DataMachineClient) deliverPendingMessage(ctx context.Context, msg PendingMessage) {
	dmc.Main.QueueRemoteText(dmc.UserLogin, msg)
	if err := dmc.AckPendingMessages(ctx, []string{msg.QueueID}); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("queue_id", msg.QueueID).Msg("Failed to acknowledge webhook-delivered message")
	}
}

func ptrStr(s string) *string { return &s }
