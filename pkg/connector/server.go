package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type CallbackServer struct {
	connector *DataMachineConnector
	server    *http.Server
	listener  net.Listener

	mu      sync.Mutex
	logins  map[string]*PKCELoginState
	started bool
}

type PKCELoginState struct {
	SiteURL      string
	CallbackURL  string
	CodeVerifier string
	State        string
	ResultCh     chan PKCECallbackResult
}

type PKCECallbackResult struct {
	Code  string
	State string
	Error string
}

func NewCallbackServer(connector *DataMachineConnector) *CallbackServer {
	return &CallbackServer{
		connector: connector,
		logins:    make(map[string]*PKCELoginState),
	}
}

func (s *CallbackServer) Start(ctx context.Context) error {
	if s.connector.Config.CallbackURL == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	mux := http.NewServeMux()
	callbackURL, err := url.Parse(s.connector.Config.CallbackURL)
	if err != nil {
		return fmt.Errorf("invalid callback URL: %w", err)
	}
	callbackPath := callbackURL.Path
	if callbackPath == "" {
		callbackPath = "/callback"
	}
	webhookPath := "/webhook"
	if strings.HasSuffix(callbackPath, "/callback") {
		webhookPath = strings.TrimSuffix(callbackPath, "/callback") + "/webhook"
	}
	mux.HandleFunc(callbackPath, s.handleCallback)
	mux.HandleFunc(webhookPath, s.handleWebhook)

	addr := fmt.Sprintf(":%d", s.connector.Config.CallbackPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.listener = listener
	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			zerolog.Ctx(ctx).Err(err).Msg("callback server stopped unexpectedly")
		}
	}()

	s.started = true
	return nil
}

func (s *CallbackServer) RegisterLogin(state string, login *PKCELoginState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logins[state] = login
}

func (s *CallbackServer) PopLogin(state string) *PKCELoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	login := s.logins[state]
	delete(s.logins, state)
	return login
}

func (s *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	login := s.PopLogin(state)
	if login == nil {
		http.Error(w, "Unknown or expired login state.", http.StatusBadRequest)
		return
	}

	result := PKCECallbackResult{
		Code:  r.URL.Query().Get("code"),
		State: state,
		Error: r.URL.Query().Get("error"),
	}

	select {
	case login.ResultCh <- result:
	default:
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(callbackPage(result.Error == "", result.Error)))
}

func (s *CallbackServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var msg PendingMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if msg.QueueID == "" || msg.SessionID == "" {
		http.Error(w, "missing queue_id or session_id", http.StatusBadRequest)
		return
	}

	if err := s.connector.HandleIncomingPendingMessage(r.Context(), msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("ok"))
}

func callbackPage(success bool, callbackError string) string {
	title := "Roadie authorization complete"
	body := "You can return to Beeper now."

	if !success {
		title = "Roadie authorization failed"
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

func (dc *DataMachineConnector) HandleIncomingPendingMessage(ctx context.Context, msg PendingMessage) error {
	if msg.SessionID == "" {
		return fmt.Errorf("missing session ID")
	}

	targets := dc.br.GetAllCachedUserLogins()
	for _, login := range targets {
		meta, ok := login.Metadata.(*UserLoginMeta)
		if !ok || meta == nil {
			continue
		}
		if meta.SiteURL != msg.SiteURL && meta.SiteURL != "" && msg.SiteURL != "" {
			continue
		}
		if meta.AgentSlug != msg.AgentSlug && meta.AgentSlug != "" && msg.AgentSlug != "" {
			continue
		}
		if meta.HasSessionID(msg.SessionID) {
			client, ok := login.Client.(*DataMachineClient)
			if !ok {
				continue
			}
			client.deliverPendingMessage(ctx, msg)
			return nil
		}
	}

	return fmt.Errorf("no login found for session %s", msg.SessionID)
}

func (dc *DataMachineConnector) ResolvePortalKey(agentSlug string, loginID networkid.UserLoginID) networkid.PortalKey {
	return networkid.PortalKey{
		ID:       networkid.PortalID("dm:" + agentSlug),
		Receiver: loginID,
	}
}

func (dc *DataMachineConnector) QueueRemoteText(login *bridgev2.UserLogin, msg PendingMessage) {
	meta, _ := login.Metadata.(*UserLoginMeta)
	if msg.AgentSlug == "" && meta != nil {
		msg.AgentSlug = meta.AgentSlug
	}
	if msg.SiteURL == "" && meta != nil {
		msg.SiteURL = meta.SiteURL
	}

	remoteEvent := &DataMachineRemoteMessage{
		portalKey: dc.ResolvePortalKey(msg.AgentSlug, login.ID),
		id:        networkid.MessageID(msg.QueueID),
		text:      msg.Content,
		agentSlug: msg.AgentSlug,
		timestamp: msg.Time(),
		sender:    EventSenderForAgent(msg.AgentSlug, login.Client.(*DataMachineClient)),
	}
	dc.br.QueueRemoteEvent(login, remoteEvent)
}
