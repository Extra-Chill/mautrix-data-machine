package bot

import (
	"context"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-datamachine/pkg/wordpress"
)

// registerHandlers sets up the Matrix event handlers on the syncer.
func (b *Bot) registerHandlers() {
	syncer := b.Client.Syncer.(*mautrix.DefaultSyncer)
	syncer.ParseEventContent = true

	// Auto-accept invites.
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		if evt.GetStateKey() != b.Client.UserID.String() {
			return
		}
		membership, ok := evt.Content.Parsed.(*event.MemberEventContent)
		if !ok || membership.Membership != event.MembershipInvite {
			return
		}
		b.Log.Info().
			Str("room_id", evt.RoomID.String()).
			Str("inviter", evt.Sender.String()).
			Msg("Received invite, auto-joining")
		_, err := b.Client.JoinRoomByID(ctx, evt.RoomID)
		if err != nil {
			b.Log.Err(err).Str("room_id", evt.RoomID.String()).Msg("Failed to join room")
		}
	})

	// Handle messages.
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		// Ignore messages from ourselves.
		if evt.Sender == b.Client.UserID {
			return
		}

		msgContent, ok := evt.Content.Parsed.(*event.MessageEventContent)
		if !ok || msgContent.Body == "" {
			return
		}

		b.handleMessage(ctx, evt, msgContent.Body)
	})
}

// handleMessage processes an incoming DM message.
func (b *Bot) handleMessage(ctx context.Context, evt *event.Event, text string) {
	roomID := evt.RoomID
	senderID := evt.Sender.String()
	portalKey := roomID.String()

	log := b.Log.With().
		Str("room_id", roomID.String()).
		Str("sender", senderID).
		Logger()

	// Logout command — clear stored token.
	if isLogoutCommand(text) {
		if err := b.UserAuth.DeleteToken(senderID); err != nil {
			log.Err(err).Msg("Failed to delete user token")
		}
		b.Sessions.ClearSession(portalKey)
		b.sendTextMessage(ctx, roomID, "You've been disconnected. Send any message to sign in again.")
		log.Info().Msg("User logged out")
		return
	}

	// Resolve per-user agent token.
	wpClient, err := b.resolveUserClient(ctx, senderID, roomID)
	if err != nil {
		log.Err(err).Msg("Failed to resolve user client")
		return
	}
	if wpClient == nil {
		// Auth flow was started; message to user already sent.
		return
	}

	// Manual session reset.
	if isResetCommand(text) {
		hadSession := b.Sessions.SessionIDForPortal(portalKey) != ""
		b.Sessions.ClearSession(portalKey)

		confirmText := "Session reset! Your next message will start a fresh conversation."
		if !hadSession {
			confirmText = "No active session to reset. Your next message will start a new conversation."
		}
		b.sendTextMessage(ctx, roomID, confirmText)
		log.Info().Bool("had_session", hadSession).Msg("User requested session reset")
		return
	}

	// Session TTL rotation.
	if b.Config.SessionIdleTTL > 0 && b.Sessions.IsSessionExpired(portalKey, b.Config.SessionIdleTTL) {
		log.Info().
			Dur("ttl", b.Config.SessionIdleTTL).
			Msg("Session idle TTL exceeded, rotating")
		b.Sessions.ClearSession(portalKey)
	}

	sessionID := b.Sessions.SessionIDForPortal(portalKey)

	// Set typing indicator.
	_, _ = b.Client.UserTyping(ctx, roomID, true, 120*time.Second)

	sendResp, err := wpClient.SendMessage(ctx, text, sessionID)
	if err != nil {
		_, _ = b.Client.UserTyping(ctx, roomID, false, 0)
		log.Err(err).Msg("Failed to send message to WordPress")

		// If the token was rejected, clear it so the user re-authenticates.
		if isAuthError(err) {
			log.Warn().Msg("Token appears invalid, clearing stored credentials")
			_ = b.UserAuth.DeleteToken(senderID)
			b.sendTextMessage(ctx, roomID, "Your session has expired. Send any message to sign in again.")
		} else {
			b.sendTextMessage(ctx, roomID, "Sorry, something went wrong processing your message.")
		}
		return
	}

	// Remember session ID.
	if sendResp.SessionID != "" {
		b.Sessions.RememberSessionID(portalKey, sendResp.SessionID)
		b.Sessions.TouchPortal(portalKey)
	}

	// Loop /chat/continue while the AI is doing tool calls.
	const maxContinueTurns = 20
	for turn := 0; !sendResp.Completed && sendResp.SessionID != "" && turn < maxContinueTurns; turn++ {
		log.Debug().Int("turn", turn+1).Str("session_id", sendResp.SessionID).Msg("AI not complete, continuing")
		contResp, contErr := wpClient.ContinueChat(ctx, sendResp.SessionID)
		if contErr != nil {
			log.Err(contErr).Msg("Failed to continue chat")
			break
		}
		sendResp.Completed = contResp.Completed
		if contResp.Response != "" {
			sendResp.Response = contResp.Response
		}
		if contResp.MessageID != "" {
			sendResp.MessageID = contResp.MessageID
		}
	}

	// Clear typing.
	_, _ = b.Client.UserTyping(ctx, roomID, false, 0)

	// Send response.
	if sendResp.Response != "" {
		b.sendMarkdownMessage(ctx, roomID, sendResp.Response)
		log.Info().Msg("Sent AI response")
	}
}

// resolveUserClient returns a per-user WordPress client if the user is authenticated.
// If the user has no stored token, it starts the PKCE auth flow and returns nil.
// Falls back to the global agent token if per-user auth is not available.
func (b *Bot) resolveUserClient(ctx context.Context, matrixUserID string, roomID id.RoomID) (*wordpress.WordPressClient, error) {
	// Try per-user token first.
	token, err := b.UserAuth.GetToken(matrixUserID)
	if err != nil {
		return nil, err
	}

	if token != "" {
		return wordpress.NewWordPressClient(
			b.Config.SiteURL,
			b.Config.AgentSlug,
			token,
			b.Config.RequestTimeout,
		), nil
	}

	// No per-user token. If callback server is available, start PKCE flow.
	if b.Callback != nil && b.Config.CallbackURL != "" {
		b.StartAuthFlow(ctx, matrixUserID, roomID)
		return nil, nil
	}

	// No callback server — fall back to global token if available.
	if b.Config.AgentToken != "" {
		return b.WP, nil
	}

	// No auth method available at all.
	b.sendTextMessage(ctx, roomID, "Authentication is required but not configured. Contact the administrator.")
	return nil, nil
}

// sendTextMessage sends a plain text message to a room.
func (b *Bot) sendTextMessage(ctx context.Context, roomID id.RoomID, text string) {
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}
	_, err := b.Client.SendMessageEvent(ctx, roomID, event.EventMessage, content)
	if err != nil {
		b.Log.Err(err).Str("room_id", roomID.String()).Msg("Failed to send text message")
	}
}

// sendMarkdownMessage renders markdown to HTML and sends a formatted message.
func (b *Bot) sendMarkdownMessage(ctx context.Context, roomID id.RoomID, text string) {
	content := format.RenderMarkdown(text, true, false)
	content.MsgType = event.MsgText
	_, err := b.Client.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
	if err != nil {
		b.Log.Err(err).Str("room_id", roomID.String()).Msg("Failed to send markdown message")
	}
}

// isResetCommand checks if a message is the /new slash command.
func isResetCommand(text string) bool {
	return strings.TrimSpace(strings.ToLower(text)) == "/new"
}

// isLogoutCommand checks if a message is the /logout slash command.
func isLogoutCommand(text string) bool {
	return strings.TrimSpace(strings.ToLower(text)) == "/logout"
}

// isAuthError checks if an error looks like an authentication/authorization failure.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
		strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "Forbidden")
}
