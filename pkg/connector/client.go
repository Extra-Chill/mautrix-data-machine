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
		"datamachine-not-logged-in":   "Please sign in to Extra Chill and authorize Roadie",
		"datamachine-invalid-auth":    "Your Roadie login is no longer valid. Please log in again",
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

	ident, err := dmc.GetIdentity(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to validate agent token")
		dmc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "datamachine-invalid-auth", Message: err.Error()})
		return
	}

	if ident.Data != nil {
		dmc.agentSlug = ident.Data.AgentSlug
		meta := dmc.UserLogin.Metadata.(*UserLoginMeta)
		meta.AgentSlug = ident.Data.AgentSlug
		meta.AgentName = ident.Data.AgentName
	}

	if dmc.Main.Config.CallbackURL != "" {
		if err := dmc.RegisterBridge(ctx); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to register bridge webhook callback")
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

func (dmc *DataMachineClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	name := portal.Name
	if name == "" {
		name = dmc.agentSlug
	}
	return &bridgev2.ChatInfo{Name: &name}, nil
}

func (dmc *DataMachineClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	name := ghost.Name
	if name == "" {
		name = dmc.agentSlug
	}
	return &bridgev2.UserInfo{Name: ptrStr(name)}, nil
}

func (dmc *DataMachineClient) GetCapabilities(_ context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	_ = portal
	return nil
}

func (dmc *DataMachineClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if !dmc.IsLoggedIn() {
		return nil, bridgev2.ErrNotLoggedIn
	}

	text := msg.Content.Body
	if msg.Content.FormattedBody != "" {
		text = msg.Content.FormattedBody
	}

	sessionID, err := dmc.ensurePortalSessionID(ctx, msg.Portal)
	if err != nil {
		return nil, err
	}

	payload := map[string]string{
		"message":    text,
		"agent":      dmc.agentSlug,
		"session_id": sessionID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat payload: %w", err)
	}

	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/datamachine/v1/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dmc.agentToken)

	resp, err := dmc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chat request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("chat request returned %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp struct {
		MessageID string `json:"message_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("failed to decode chat response: %w", err)
	}

	msgID := networkid.MessageID(chatResp.MessageID)
	if msgID == "" {
		msgID = networkid.MessageID(msg.Event.ID)
	}

	return &bridgev2.MatrixMessageResponse{DB: &database.Message{ID: msgID}}, nil
}

func (dmc *DataMachineClient) GetIdentity(ctx context.Context) (*IdentityResponse, error) {
	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/chat-bridge/v1/identity"
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

	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/chat-bridge/v1/register"
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
	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/chat-bridge/v1/pending"
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

	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/chat-bridge/v1/ack"
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
