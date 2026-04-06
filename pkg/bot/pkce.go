package bot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-datamachine/pkg/wordpress"
)

// pendingLogin tracks a PKCE authorization in progress for a Matrix user.
type pendingLogin struct {
	MatrixUserID string
	RoomID       id.RoomID
	CodeVerifier string
	State        string
	CreatedAt    time.Time
}

// BotCallbackServer handles PKCE callbacks for bot-mode per-user auth.
type BotCallbackServer struct {
	bot      *Bot
	server   *http.Server
	listener net.Listener

	mu       sync.Mutex
	logins   map[string]*pendingLogin // state → pending login
	started  bool
}

// newBotCallbackServer creates a callback server bound to the bot.
func newBotCallbackServer(b *Bot) *BotCallbackServer {
	return &BotCallbackServer{
		bot:    b,
		logins: make(map[string]*pendingLogin),
	}
}

// Start begins listening for PKCE callbacks. Safe to call multiple times.
func (s *BotCallbackServer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	callbackURL := s.bot.Config.CallbackURL
	if callbackURL == "" {
		return fmt.Errorf("callback_url is required for per-user auth")
	}

	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return fmt.Errorf("invalid callback URL: %w", err)
	}
	callbackPath := parsed.Path
	if callbackPath == "" {
		callbackPath = "/callback"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, s.handleCallback)

	port := s.bot.Config.CallbackPort
	if port == 0 {
		port = 29340
	}
	addr := fmt.Sprintf(":%d", port)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.listener = listener
	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Graceful shutdown on context cancellation.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.bot.Log.Err(err).Msg("Bot callback server stopped unexpectedly")
		}
	}()

	s.started = true
	s.bot.Log.Info().Str("addr", addr).Msg("Bot callback server started")
	return nil
}

// RegisterLogin stores a pending PKCE login keyed by state.
func (s *BotCallbackServer) RegisterLogin(state string, login *pendingLogin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logins[state] = login
}

// PopLogin retrieves and removes a pending login by state.
func (s *BotCallbackServer) PopLogin(state string) *pendingLogin {
	s.mu.Lock()
	defer s.mu.Unlock()
	login := s.logins[state]
	delete(s.logins, state)
	return login
}

// handleCallback processes the PKCE redirect from WordPress.
func (s *BotCallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	callbackError := r.URL.Query().Get("error")

	login := s.PopLogin(state)
	if login == nil {
		http.Error(w, "Unknown or expired login state.", http.StatusBadRequest)
		return
	}

	log := s.bot.Log.With().
		Str("matrix_user_id", login.MatrixUserID).
		Str("room_id", login.RoomID.String()).
		Logger()

	if callbackError != "" {
		log.Warn().Str("error", callbackError).Msg("PKCE authorization failed")
		s.bot.sendTextMessage(context.Background(), login.RoomID,
			fmt.Sprintf("Authorization failed: %s. Send any message to try again.", callbackError))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(botCallbackPage(false, callbackError)))
		return
	}

	if code == "" {
		log.Warn().Msg("PKCE callback missing code")
		s.bot.sendTextMessage(context.Background(), login.RoomID,
			"Authorization failed: no code received. Send any message to try again.")
		http.Error(w, "Missing authorization code.", http.StatusBadRequest)
		return
	}

	// Exchange code for token.
	ctx, cancel := context.WithTimeout(context.Background(), s.bot.Config.RequestTimeout)
	defer cancel()

	tokenResp, err := s.exchangeCode(ctx, code, login.CodeVerifier)
	if err != nil {
		log.Err(err).Msg("Failed to exchange authorization code")
		s.bot.sendTextMessage(context.Background(), login.RoomID,
			"Authorization failed during token exchange. Send any message to try again.")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(botCallbackPage(false, "token exchange failed")))
		return
	}

	// Verify agent access.
	if err := s.verifyAccess(ctx, tokenResp.AccessToken); err != nil {
		log.Err(err).Msg("Agent access verification failed")
		s.bot.sendTextMessage(context.Background(), login.RoomID,
			"You don't have access to this agent. Contact the site administrator.")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(botCallbackPage(false, "access denied")))
		return
	}

	// Store token.
	if err := s.bot.UserAuth.SaveToken(login.MatrixUserID, tokenResp.AccessToken, s.bot.Config.SiteURL); err != nil {
		log.Err(err).Msg("Failed to save user token")
		s.bot.sendTextMessage(context.Background(), login.RoomID,
			"Authorization succeeded but failed to save your credentials. Please try again.")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(botCallbackPage(false, "storage error")))
		return
	}

	log.Info().Str("agent_slug", tokenResp.AgentSlug).Msg("User authenticated successfully")

	// Send confirmation — use welcome message from onboarding if available.
	confirmMsg := "You're connected! Send me a message to start chatting."
	onboarding, onErr := s.bot.WP.GetOnboarding(ctx, s.bot.Config.AgentSlug)
	if onErr == nil && onboarding != nil && onboarding.Data != nil && onboarding.Data.WelcomeMessage != "" {
		confirmMsg = onboarding.Data.WelcomeMessage
	}
	s.bot.sendMarkdownMessage(context.Background(), login.RoomID, confirmMsg)

	// Render success page in the browser.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(botCallbackPage(true, "")))
}

// exchangeCode trades an authorization code for an agent token.
func (s *BotCallbackServer) exchangeCode(ctx context.Context, code, codeVerifier string) (*wordpress.TokenExchangeResponse, error) {
	payload := map[string]string{
		"code":          code,
		"code_verifier": codeVerifier,
		"redirect_uri":  s.bot.Config.CallbackURL,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	apiURL := strings.TrimRight(s.bot.Config.SiteURL, "/") + "/wp-json/datamachine/v1/bridge/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := wordpress.LocalClient(s.bot.Config.RequestTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp wordpress.TokenExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}
	return &tokenResp, nil
}

// verifyAccess checks that the token grants access to the agent.
func (s *BotCallbackServer) verifyAccess(ctx context.Context, token string) error {
	apiURL := strings.TrimRight(s.bot.Config.SiteURL, "/") + "/wp-json/datamachine/v1/bridge/identity"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := wordpress.LocalClient(s.bot.Config.RequestTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("access check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("access denied")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("access check returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// StartAuthFlow initiates the PKCE flow for a Matrix user. It sends an
// authorization link to the user's room and registers the pending login
// on the callback server.
func (b *Bot) StartAuthFlow(ctx context.Context, matrixUserID string, roomID id.RoomID) {
	log := b.Log.With().
		Str("matrix_user_id", matrixUserID).
		Str("room_id", roomID.String()).
		Logger()

	verifier, err := generateRandomString(32)
	if err != nil {
		log.Err(err).Msg("Failed to generate PKCE verifier")
		b.sendTextMessage(ctx, roomID, "Internal error starting authentication. Please try again.")
		return
	}

	state, err := generateRandomString(24)
	if err != nil {
		log.Err(err).Msg("Failed to generate PKCE state")
		b.sendTextMessage(ctx, roomID, "Internal error starting authentication. Please try again.")
		return
	}

	challenge := pkceChallenge(verifier)

	authURL, err := buildAuthorizeURL(b.Config.SiteURL, b.Config.AgentSlug, b.Config.CallbackURL, challenge, state)
	if err != nil {
		log.Err(err).Msg("Failed to build authorize URL")
		b.sendTextMessage(ctx, roomID, "Internal error starting authentication. Please try again.")
		return
	}

	pending := &pendingLogin{
		MatrixUserID: matrixUserID,
		RoomID:       roomID,
		CodeVerifier: verifier,
		State:        state,
		CreatedAt:    time.Now(),
	}
	b.Callback.RegisterLogin(state, pending)

	// Fetch login instructions from onboarding metadata.
	instructions := "To chat with me, sign in first:"
	onboarding, onErr := b.WP.GetOnboarding(ctx, b.Config.AgentSlug)
	if onErr == nil && onboarding != nil && onboarding.Data != nil && onboarding.Data.LoginInstructions != "" {
		instructions = onboarding.Data.LoginInstructions
	}

	msg := fmt.Sprintf("%s\n\n%s", instructions, authURL)
	b.sendTextMessage(ctx, roomID, msg)

	log.Info().Str("state", state).Msg("Started PKCE auth flow for user")
}

// generateRandomString produces a URL-safe base64 random string.
func generateRandomString(byteLength int) (string, error) {
	buf := make([]byte, byteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkceChallenge computes the S256 PKCE challenge from a verifier.
func pkceChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// buildAuthorizeURL constructs the WordPress agent authorization URL.
func buildAuthorizeURL(siteURL, agentSlug, callbackURL, challenge, state string) (string, error) {
	base := strings.TrimRight(siteURL, "/") + "/wp-json/datamachine/v1/agent/authorize"
	params := url.Values{}
	params.Set("agent_slug", agentSlug)
	params.Set("redirect_uri", callbackURL)
	params.Set("label", fmt.Sprintf("matrix-%s", agentSlug))
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	return base + "?" + params.Encode(), nil
}

// botCallbackPage renders a simple HTML page shown in the browser after the PKCE callback.
func botCallbackPage(success bool, callbackError string) string {
	title := "Authorization complete"
	body := "You can return to your chat app now."

	if !success {
		title = "Authorization failed"
		body = fmt.Sprintf("Authorization failed: %s", html.EscapeString(callbackError))
	}

	return strings.TrimSpace(fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>%s</title>
  <style>
    body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;padding:2rem}
    .card{max-width:32rem;background:#111827;border:1px solid #334155;border-radius:16px;padding:2rem;box-shadow:0 20px 40px rgba(0,0,0,.35)}
    h1{margin:0 0 1rem;font-size:1.5rem}
    p{margin:0;color:#cbd5e1;line-height:1.5}
  </style>
</head>
<body>
  <div class="card">
    <h1>%s</h1>
    <p>%s</p>
  </div>
</body>
</html>`, title, title, body))
}


