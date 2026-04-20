package bot

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
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

	// Handle messages — dispatch by MsgType so text, images and
	// unsupported types each take their own path.
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		// Ignore messages from ourselves.
		if evt.Sender == b.Client.UserID {
			return
		}

		msgContent, ok := evt.Content.Parsed.(*event.MessageEventContent)
		if !ok {
			return
		}

		switch msgContent.MsgType {
		case event.MsgText, event.MsgEmote, event.MsgNotice:
			if msgContent.Body == "" {
				return
			}
			b.handleTextMessage(ctx, evt, msgContent.Body)

		case event.MsgImage:
			b.handleImageMessage(ctx, evt, msgContent)

		default:
			// Video, audio, file, location etc. are deferred to future work.
			// Silently ignore so unknown types don't spam users with errors.
			b.Log.Debug().
				Str("msg_type", string(msgContent.MsgType)).
				Str("event_id", evt.ID.String()).
				Msg("Ignoring unsupported message type")
		}
	})
}

// handleTextMessage processes a plain text DM. It handles the slash
// commands (/logout, /new) that only make sense for text, then hands
// off to runChatTurn for the shared AI-turn logic.
func (b *Bot) handleTextMessage(ctx context.Context, evt *event.Event, text string) {
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

	// Resolve per-user agent token (starts PKCE flow if needed).
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

	b.runChatTurn(ctx, evt, wpClient, text, nil, log)
}

// handleImageMessage processes an m.image event: download the Matrix
// media (E2EE-safe), upload to the WordPress Media Library, then call
// runChatTurn with the upload URL as an attachment so the AI can see
// the image and hand it to socials publish tools (publish_instagram etc.).
func (b *Bot) handleImageMessage(ctx context.Context, evt *event.Event, mc *event.MessageEventContent) {
	roomID := evt.RoomID
	senderID := evt.Sender.String()

	log := b.Log.With().
		Str("room_id", roomID.String()).
		Str("sender", senderID).
		Str("event_id", evt.ID.String()).
		Logger()

	if !b.Config.Media.Enabled {
		log.Debug().Msg("Media forwarding disabled; dropping m.image event")
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

	// Typing indicator for the whole download+upload+turn duration.
	_, _ = b.Client.UserTyping(ctx, roomID, true, 120*time.Second)
	clearTyping := func() { _, _ = b.Client.UserTyping(ctx, roomID, false, 0) }

	// Pre-upload size check against the Info field (server-advertised),
	// to reject obvious monsters before spending bytes on a download.
	if mc.Info != nil && b.Config.Media.MaxBytes > 0 && int64(mc.Info.Size) > b.Config.Media.MaxBytes {
		clearTyping()
		b.sendTextMessage(ctx, roomID, fmt.Sprintf(
			"That image is too large (%d bytes). Max is %d bytes — try resizing.",
			mc.Info.Size, b.Config.Media.MaxBytes,
		))
		log.Warn().Int("size", mc.Info.Size).Msg("Rejecting oversize image (Info.Size)")
		return
	}

	// Download with a per-download timeout.
	downloadCtx, cancelDL := context.WithTimeout(ctx, b.Config.Media.DownloadTimeout)
	defer cancelDL()

	data, err := b.downloadMatrixMedia(downloadCtx, mc)
	if err != nil {
		clearTyping()
		log.Err(err).Msg("Failed to download Matrix media")
		b.sendTextMessage(ctx, roomID, "Sorry, I couldn't download that image.")
		return
	}

	// Post-download size check (trust but verify — Info.Size can lie).
	if b.Config.Media.MaxBytes > 0 && int64(len(data)) > b.Config.Media.MaxBytes {
		clearTyping()
		b.sendTextMessage(ctx, roomID, fmt.Sprintf(
			"That image is too large (%d bytes). Max is %d bytes — try resizing.",
			len(data), b.Config.Media.MaxBytes,
		))
		log.Warn().Int("size", len(data)).Msg("Rejecting oversize image (post-download)")
		return
	}

	// Mime type: prefer Info.MimeType, fall back to sniffing the first 512 bytes.
	mimeType := ""
	if mc.Info != nil {
		mimeType = mc.Info.MimeType
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	// Normalize: DetectContentType can return "image/jpeg; charset=..." occasionally;
	// keep only the type/subtype.
	if i := strings.Index(mimeType, ";"); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}

	if !b.isAllowedMimeType(mimeType) {
		clearTyping()
		b.sendTextMessage(ctx, roomID, fmt.Sprintf(
			"Sorry, %q isn't a supported image type. Try JPEG, PNG or WebP.",
			mimeType,
		))
		log.Warn().Str("mime", mimeType).Msg("Rejecting unsupported mime type")
		return
	}

	// Derive filename. mc.Body is usually the filename for m.image events
	// (e.g. "IMG_1234.jpeg") but can also be a caption. If it's empty or
	// looks default, synthesize from event ID + mime.
	filename := deriveUploadFilename(mc, evt.ID.String(), mimeType)

	// Upload to WP media library, bounded by its own timeout.
	uploadCtx, cancelUL := context.WithTimeout(ctx, b.Config.Media.UploadTimeout)
	defer cancelUL()

	uploadResp, err := wpClient.UploadMedia(uploadCtx, data, filename, mimeType)
	if err != nil {
		clearTyping()
		log.Err(err).Msg("Failed to upload media to WordPress")

		if isAuthError(err) {
			// Treat auth errors the same way the text path does — clear the
			// token so the user re-authenticates on their next message.
			_ = b.UserAuth.DeleteToken(senderID)
			b.sendTextMessage(ctx, roomID, "Your session has expired. Send any message to sign in again.")
		} else {
			b.sendTextMessage(ctx, roomID, "Sorry, I couldn't attach your image.")
		}
		return
	}

	log.Info().
		Int64("media_id", uploadResp.ID).
		Str("url", uploadResp.SourceURL).
		Str("mime", uploadResp.MimeType).
		Int("bytes", len(data)).
		Msg("Uploaded Matrix media to WordPress")

	// Build caption. For m.image the Body is usually the filename; when
	// that's the case we send an empty caption so the AI doesn't think
	// "IMG_1234.jpeg" is user intent.
	caption := strings.TrimSpace(mc.Body)
	if looksLikeMediaFilename(caption) {
		caption = ""
	}

	attachments := []wordpress.Attachment{
		{
			URL:      uploadResp.SourceURL,
			MediaID:  uploadResp.ID,
			MimeType: uploadResp.MimeType,
			Filename: filename,
		},
	}

	b.runChatTurn(ctx, evt, wpClient, caption, attachments, log)
}

// runChatTurn is the shared AI-turn loop for both text and image inputs.
// It handles session TTL rotation, session bookkeeping, the initial
// /bridge/send call, the /chat/continue loop while the AI makes tool
// calls, and the final markdown reply to the Matrix room.
func (b *Bot) runChatTurn(
	ctx context.Context,
	evt *event.Event,
	wpClient *wordpress.WordPressClient,
	message string,
	attachments []wordpress.Attachment,
	log zerolog.Logger,
) {
	roomID := evt.RoomID
	senderID := evt.Sender.String()
	portalKey := roomID.String()

	// Session TTL rotation.
	if b.Config.SessionIdleTTL > 0 && b.Sessions.IsSessionExpired(portalKey, b.Config.SessionIdleTTL) {
		log.Info().
			Dur("ttl", b.Config.SessionIdleTTL).
			Msg("Session idle TTL exceeded, rotating")
		b.Sessions.ClearSession(portalKey)
	}

	sessionID := b.Sessions.SessionIDForPortal(portalKey)

	// Typing indicator (idempotent if handleImageMessage already set one).
	_, _ = b.Client.UserTyping(ctx, roomID, true, 120*time.Second)

	bridgeCtx := b.deriveBridgeContext(evt)

	sendResp, err := wpClient.SendMessage(ctx, message, sessionID, attachments, bridgeCtx)
	if err != nil {
		_, _ = b.Client.UserTyping(ctx, roomID, false, 0)
		log.Err(err).Msg("Failed to send message to WordPress")

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

// downloadMatrixMedia retrieves the bytes for an m.image event. For
// encrypted rooms (mc.File != nil) we pull the ciphertext via the
// homeserver and run it through the attachment's Decrypt. For clear
// rooms (mc.URL set) we use Client.DownloadBytes directly.
func (b *Bot) downloadMatrixMedia(ctx context.Context, mc *event.MessageEventContent) ([]byte, error) {
	// Encrypted rooms: mc.File carries the mxc URL + JWK key + IV.
	if mc.File != nil {
		mxcURL, err := mc.File.URL.Parse()
		if err != nil {
			return nil, fmt.Errorf("parse encrypted mxc url: %w", err)
		}
		ciphertext, err := b.Client.DownloadBytes(ctx, mxcURL)
		if err != nil {
			return nil, fmt.Errorf("download encrypted media: %w", err)
		}
		plaintext, err := mc.File.Decrypt(ciphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt media: %w", err)
		}
		return plaintext, nil
	}

	// Clear rooms: mc.URL is an mxc:// content URI string.
	if mc.URL != "" {
		mxcURL, err := mc.URL.Parse()
		if err != nil {
			return nil, fmt.Errorf("parse mxc url: %w", err)
		}
		return b.Client.DownloadBytes(ctx, mxcURL)
	}

	return nil, fmt.Errorf("m.image event has neither URL nor encrypted File")
}

// isAllowedMimeType checks the configured allowlist, case-insensitive.
func (b *Bot) isAllowedMimeType(mime string) bool {
	if mime == "" {
		return false
	}
	mime = strings.ToLower(mime)
	for _, allowed := range b.Config.Media.AllowedMimeTypes {
		if strings.ToLower(allowed) == mime {
			return true
		}
	}
	return false
}

// deriveBridgeContext best-effort extracts the upstream app / room /
// room-kind from a Matrix event. For a mautrix bridge bot sitting in
// Beeper-forwarded rooms, the canonical signals we have without
// upstream-specific metadata are:
//
//  1. Bot is always in "bridge" context (bridge_app="matrix")
//  2. Room kind defaults to "dm" — Beeper DMs are 1:1 rooms; future
//     work can detect group/channel by room member count.
//  3. bridge_room is the Matrix room ID (opaque, stable, useful for
//     DM-side mode guidance).
//
// Returns nil when nothing meaningful can be derived (keeps the
// payload lean for tests / direct Matrix users).
func (b *Bot) deriveBridgeContext(evt *event.Event) *wordpress.BridgeContext {
	if evt == nil {
		return nil
	}
	return &wordpress.BridgeContext{
		App:      "matrix",
		Room:     evt.RoomID.String(),
		RoomKind: "dm",
	}
}

// deriveUploadFilename picks a filename for the WP media upload.
// Prefers mc.FileName when set (newer mautrix spec extension), falls
// back to mc.Body, and finally synthesizes "<event-id>.<ext>" when
// neither is a sensible filename.
func deriveUploadFilename(mc *event.MessageEventContent, eventID, mimeType string) string {
	candidates := []string{mc.FileName, mc.Body}
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		// Keep only the base to strip any accidental paths.
		base := filepath.Base(name)
		if looksLikeMediaFilename(base) {
			return base
		}
	}

	// Synthesize.
	ext := extFromMime(mimeType)
	// event IDs start with "$" and can contain "/" and ":"; sanitize.
	safe := strings.NewReplacer("$", "", "/", "_", ":", "_").Replace(eventID)
	if safe == "" {
		safe = fmt.Sprintf("matrix-%d", time.Now().Unix())
	}
	return safe + ext
}

// looksLikeMediaFilename returns true when the given string looks like
// "something.ext" — used to decide if mc.Body should be treated as a
// filename (discard) or a caption (pass through to the AI).
func looksLikeMediaFilename(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// No spaces, has a dot, has a short extension — classic filename shape.
	if strings.ContainsAny(s, " \t\n") {
		return false
	}
	ext := filepath.Ext(s)
	return len(ext) >= 2 && len(ext) <= 6
}

// extFromMime returns a leading-dot extension for a known image mime
// type, or an empty string (caller falls back to no extension).
func extFromMime(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ""
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
