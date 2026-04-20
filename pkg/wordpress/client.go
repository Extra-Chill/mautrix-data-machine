package wordpress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WordPressClient handles all HTTP communication with the WordPress/Data Machine REST API.
type WordPressClient struct {
	SiteURL        string
	AgentSlug      string
	AgentToken     string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

// NewWordPressClient creates a client with sensible defaults.
func NewWordPressClient(siteURL, agentSlug, agentToken string, requestTimeout time.Duration) *WordPressClient {
	if requestTimeout == 0 {
		requestTimeout = 120 * time.Second
	}
	return &WordPressClient{
		SiteURL:        strings.TrimRight(siteURL, "/"),
		AgentSlug:      agentSlug,
		AgentToken:     agentToken,
		HTTPClient:     HTTPClientWithTimeout(requestTimeout),
		RequestTimeout: requestTimeout,
	}
}

// SendMessage sends a user message via the bridge /send endpoint.
//
// Attachments (optional) mirror the /datamachine/v1/chat schema and are
// passed straight through to the datamachine/send-message ability, which
// hands them to ChatOrchestrator for multi-modal conversation building.
//
// BridgeCtx (optional) carries per-room metadata (app/room/room-kind) so
// the agent's bridge-mode guidance can adapt to iMessage vs WhatsApp vs
// DM vs group contexts.
func (c *WordPressClient) SendMessage(
	ctx context.Context,
	message, sessionID string,
	attachments []Attachment,
	bridgeCtx *BridgeContext,
) (*SendResponse, error) {
	// Use interface{} so we can mix strings and the attachments slice.
	payload := map[string]interface{}{
		"message": message,
	}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	if bridgeCtx != nil {
		if bridgeCtx.App != "" {
			payload["bridge_app"] = bridgeCtx.App
		}
		if bridgeCtx.Room != "" {
			payload["bridge_room"] = bridgeCtx.Room
		}
		if bridgeCtx.RoomKind != "" {
			payload["bridge_room_kind"] = bridgeCtx.RoomKind
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal send payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), c.RequestTimeout)
	defer cancel()

	apiURL := c.SiteURL + "/wp-json/datamachine/v1/bridge/send"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AgentToken)

	resp, err := LocalClient(c.RequestTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("send returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Ignore the unused variable from ctx — it's used only for caller tracing.
	_ = ctx

	var sendResp SendResponse
	if err := json.NewDecoder(resp.Body).Decode(&sendResp); err != nil {
		return nil, fmt.Errorf("failed to decode send response: %w", err)
	}

	return &sendResp, nil
}

// UploadMedia uploads raw bytes to the WordPress Media Library via the
// REST API (/wp/v2/media) using the agent bearer token. Returns the
// attachment ID and the public `source_url` suitable for forwarding
// to /bridge/send as an Attachment.URL or for direct consumption by
// social publish tools (Instagram Graph API requires public https URLs).
//
// The caller is responsible for enforcing size/mime limits before
// calling this — this method trusts its inputs.
func (c *WordPressClient) UploadMedia(
	ctx context.Context,
	data []byte,
	filename, mimeType string,
) (*MediaUploadResponse, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("upload data is empty")
	}
	if mimeType == "" {
		return nil, fmt.Errorf("mime type is required")
	}
	if filename == "" {
		filename = "upload.bin"
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), c.RequestTimeout)
	defer cancel()

	apiURL := c.SiteURL + "/wp-json/wp/v2/media"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, apiURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create media upload request: %w", err)
	}
	req.Header.Set("Content-Type", mimeType)
	// `Content-Disposition: attachment; filename="..."` is how WP's REST
	// media controller discovers the filename when no multipart/form-data
	// wrapper is used. Single-shot binary upload is simpler and avoids
	// the multipart overhead.
	req.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	req.Header.Set("Authorization", "Bearer "+c.AgentToken)

	resp, err := LocalClient(c.RequestTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("media upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("media upload returned %d: %s", resp.StatusCode, string(respBody))
	}

	_ = ctx

	var uploadResp MediaUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return nil, fmt.Errorf("failed to decode media upload response: %w", err)
	}

	if uploadResp.ID == 0 || uploadResp.SourceURL == "" {
		return nil, fmt.Errorf("media upload succeeded but response is missing id or source_url")
	}

	return &uploadResp, nil
}

// ContinueChat calls /chat/continue to resume a multi-turn AI conversation.
func (c *WordPressClient) ContinueChat(ctx context.Context, sessionID string) (*SendResponse, error) {
	payload := map[string]string{
		"session_id": sessionID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal continue payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), c.RequestTimeout)
	defer cancel()

	apiURL := c.SiteURL + "/wp-json/datamachine/v1/chat/continue"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create continue request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AgentToken)

	resp, err := LocalClient(c.RequestTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("continue request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("continue returned %d: %s", resp.StatusCode, string(respBody))
	}

	_ = ctx

	var wrapper struct {
		Success bool         `json:"success"`
		Data    SendResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("failed to decode continue response: %w", err)
	}

	return &wrapper.Data, nil
}

// GetIdentity validates the agent token and returns agent identity.
func (c *WordPressClient) GetIdentity(ctx context.Context) (*IdentityResponse, error) {
	apiURL := c.SiteURL + "/wp-json/datamachine/v1/bridge/identity"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AgentToken)

	resp, err := c.HTTPClient.Do(req)
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

// GetOnboarding fetches onboarding metadata from the WordPress site.
func (c *WordPressClient) GetOnboarding(ctx context.Context, agentSlug string) (*OnboardingResponse, error) {
	apiURL := c.SiteURL + "/wp-json/datamachine/v1/bridge/onboarding"
	if agentSlug != "" {
		apiURL += "?agent_slug=" + agentSlug
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	if c.AgentToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AgentToken)
	}

	resp, err := c.HTTPClient.Do(req)
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

// RegisterBridge registers the bridge callback URL with WordPress.
func (c *WordPressClient) RegisterBridge(ctx context.Context, callbackURL, bridgeID string) error {
	payload := map[string]string{
		"callback_url": callbackURL,
		"bridge_id":    bridgeID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	apiURL := c.SiteURL + "/wp-json/datamachine/v1/bridge/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AgentToken)

	resp, err := c.HTTPClient.Do(req)
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

// GetPendingMessages fetches pending messages, optionally filtered by session IDs.
func (c *WordPressClient) GetPendingMessages(ctx context.Context, sessionIDs []string) ([]PendingMessage, error) {
	apiURL := c.SiteURL + "/wp-json/datamachine/v1/bridge/pending"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AgentToken)

	if len(sessionIDs) > 0 {
		query := req.URL.Query()
		for _, sid := range sessionIDs {
			query.Add("session_ids", sid)
		}
		req.URL.RawQuery = query.Encode()
	}

	resp, err := c.HTTPClient.Do(req)
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
			envelope.Messages[i].AgentSlug = c.AgentSlug
		}
		if envelope.Messages[i].SiteURL == "" {
			envelope.Messages[i].SiteURL = c.SiteURL
		}
	}
	return envelope.Messages, nil
}

// AckPendingMessages acknowledges delivered messages by their queue IDs.
func (c *WordPressClient) AckPendingMessages(ctx context.Context, ids []string) error {
	payload := map[string][]string{"message_ids": ids}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	apiURL := c.SiteURL + "/wp-json/datamachine/v1/bridge/ack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AgentToken)

	resp, err := c.HTTPClient.Do(req)
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
