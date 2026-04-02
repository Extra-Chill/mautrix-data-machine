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

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

func init() {
	status.BridgeStateHumanErrors.Update(status.BridgeStateErrorMap{
		"datamachine-not-logged-in": "Please log in with your site URL and agent token",
		"datamachine-invalid-auth":  "Invalid agent token, please log in again",
	})
}

// DataMachineClient implements bridgev2.NetworkAPI for a single Data Machine agent login.
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

var (
	_ bridgev2.NetworkAPI = (*DataMachineClient)(nil)
)

func (dmc *DataMachineClient) Connect(ctx context.Context) {
	if dmc.agentToken == "" || dmc.siteURL == "" {
		dmc.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "datamachine-not-logged-in",
		})
		return
	}

	// Validate the token by calling the identity endpoint.
	_, err := dmc.GetIdentity(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to validate agent token")
		dmc.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "datamachine-invalid-auth",
			Message:    err.Error(),
		})
		return
	}

	// Start polling for pending messages.
	go dmc.startPolling()

	dmc.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateConnected,
	})
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
	return networkid.UserID(dmc.agentSlug+"@"+dmc.siteURL) == userID
}

func (dmc *DataMachineClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	// Each portal is a 1:1 DM with a specific agent. Return basic info.
	return &bridgev2.ChatInfo{
		Name: &portal.Name,
	}, nil
}

func (dmc *DataMachineClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	// Return info about the agent based on ghost metadata.
	name := ghost.Name
	if name == "" {
		name = dmc.agentSlug
	}
	return &bridgev2.UserInfo{
		Name: ptrStr(name),
	}, nil
}

func (dmc *DataMachineClient) GetCapabilities(_ context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	// This bridge is text-only DMs for now.
	return nil
}

// HandleMatrixMessage sends a Matrix message to the Data Machine agent via WordPress REST API.
func (dmc *DataMachineClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if !dmc.IsLoggedIn() {
		return nil, bridgev2.ErrNotLoggedIn
	}

	// Extract the text content from the Matrix message.
	text := msg.Content.Body
	if msg.Content.FormattedBody != "" {
		text = msg.Content.FormattedBody
	}

	// POST to /wp-json/datamachine/v1/chat
	payload := map[string]string{
		"message":   text,
		"agent":     dmc.agentSlug,
		"session_id": string(msg.Event.ID),
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

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID: msgID,
		},
	}, nil
}

// --- WordPress REST API helpers ---

// IdentityResponse represents the response from /chat-bridge/v1/identity.
type IdentityResponse struct {
	AgentSlug string `json:"agent_slug"`
	AgentName string `json:"agent_name"`
	SiteURL   string `json:"site_url"`
}

// GetIdentity validates the agent token and returns identity info.
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
	return &ident, nil
}

// PendingMessage represents a pending message from the WordPress poll endpoint.
type PendingMessage struct {
	ID        string `json:"id"`
	Message   string `json:"message"`
	AgentSlug string `json:"agent_slug"`
	Timestamp int64  `json:"timestamp"`
}

// GetPendingMessages polls /chat-bridge/v1/pending for undelivered messages.
func (dmc *DataMachineClient) GetPendingMessages(ctx context.Context) ([]PendingMessage, error) {
	url := strings.TrimRight(dmc.siteURL, "/") + "/wp-json/chat-bridge/v1/pending"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+dmc.agentToken)

	resp, err := dmc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pending request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pending returned %d: %s", resp.StatusCode, string(body))
	}

	var messages []PendingMessage
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil, fmt.Errorf("failed to decode pending messages: %w", err)
	}
	return messages, nil
}

// AckPendingMessages acknowledges delivered messages via /chat-bridge/v1/ack.
func (dmc *DataMachineClient) AckPendingMessages(ctx context.Context, ids []string) error {
	payload := map[string][]string{"ids": ids}
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

func ptrStr(s string) *string {
	return &s
}
