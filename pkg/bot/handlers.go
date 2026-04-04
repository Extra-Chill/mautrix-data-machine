package bot

import (
	"context"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
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
	portalKey := roomID.String()

	log := b.Log.With().
		Str("room_id", roomID.String()).
		Str("sender", evt.Sender.String()).
		Logger()

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

	sendResp, err := b.WP.SendMessage(ctx, text, sessionID)
	if err != nil {
		_, _ = b.Client.UserTyping(ctx, roomID, false, 0)
		log.Err(err).Msg("Failed to send message to WordPress")
		b.sendTextMessage(ctx, roomID, "Sorry, something went wrong processing your message.")
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
		contResp, contErr := b.WP.ContinueChat(ctx, sendResp.SessionID)
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
