package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-datamachine/pkg/wordpress"
)

// Bot is a regular Matrix client that relays DMs to a WordPress/Data Machine agent.
type Bot struct {
	Client       *mautrix.Client
	CryptoHelper *cryptohelper.CryptoHelper
	WP           *wordpress.WordPressClient
	Sessions     *wordpress.SessionStore
	UserAuth     *UserAuth
	Callback     *BotCallbackServer
	Config       BotConfig
	Log          zerolog.Logger
}

// BotConfig holds all configuration for bot mode.
type BotConfig struct {
	HomeserverURL  string        `yaml:"homeserver_url"`
	UserID         string        `yaml:"user_id"`
	Password       string        `yaml:"password"`
	AccessToken    string        `yaml:"access_token"`
	SiteURL        string        `yaml:"site_url"`
	AgentSlug      string        `yaml:"agent_slug"`
	AgentToken     string        `yaml:"agent_token"`
	SessionIdleTTL time.Duration `yaml:"session_idle_ttl"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	PickleKey      string        `yaml:"pickle_key"`
	DatabasePath   string        `yaml:"database_path"`

	// Per-user PKCE auth fields.
	CallbackURL      string `yaml:"callback_url"`
	CallbackPort     int    `yaml:"callback_port"`
	AuthDatabasePath string `yaml:"auth_database_path"`

	// Media forwarding (m.image events → WP Media Library → /bridge/send).
	Media MediaConfig `yaml:"media"`
}

// MediaConfig controls how inbound Matrix media events are forwarded
// to WordPress. Zero-values fall back to sane defaults (see applyMediaDefaults).
type MediaConfig struct {
	// Enabled turns image forwarding on/off entirely. When false,
	// m.image events are silently dropped (matching historical behavior).
	Enabled bool `yaml:"enabled"`

	// MaxBytes is the hard cap on a single image upload. Instagram's
	// Graph API limit is 8 MB for feed images; default matches that.
	MaxBytes int64 `yaml:"max_bytes"`

	// AllowedMimeTypes is an allowlist; anything else is rejected with
	// a user-facing error. Defaults to image/jpeg, image/png, image/webp
	// (the set Instagram Graph API accepts).
	AllowedMimeTypes []string `yaml:"allowed_mime_types"`

	// DownloadTimeout bounds a single Matrix media download.
	DownloadTimeout time.Duration `yaml:"download_timeout"`

	// UploadTimeout bounds a single WP /wp/v2/media upload.
	UploadTimeout time.Duration `yaml:"upload_timeout"`
}

// applyMediaDefaults fills in zero-value MediaConfig fields with the
// values we'd want most of the time. Callers can override any field
// in bot-config.yaml without losing the others.
func applyMediaDefaults(m *MediaConfig) {
	if m.MaxBytes == 0 {
		// 8 MB matches Instagram Graph API's feed image cap.
		m.MaxBytes = 8 * 1024 * 1024
	}
	if len(m.AllowedMimeTypes) == 0 {
		m.AllowedMimeTypes = []string{"image/jpeg", "image/png", "image/webp"}
	}
	if m.DownloadTimeout == 0 {
		m.DownloadTimeout = 30 * time.Second
	}
	if m.UploadTimeout == 0 {
		m.UploadTimeout = 60 * time.Second
	}
}

// Run starts the bot: logs in, sets up E2EE, registers handlers, and syncs.
// It blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if b.Config.HomeserverURL == "" || b.Config.UserID == "" {
		return fmt.Errorf("homeserver_url and user_id are required")
	}
	if b.Config.SiteURL == "" {
		return fmt.Errorf("site_url is required")
	}

	// Defaults.
	if b.Config.RequestTimeout == 0 {
		b.Config.RequestTimeout = 120 * time.Second
	}
	if b.Config.SessionIdleTTL == 0 {
		b.Config.SessionIdleTTL = 24 * time.Hour
	}
	if b.Config.DatabasePath == "" {
		b.Config.DatabasePath = "./bot-crypto.db"
	}
	if b.Config.CallbackPort == 0 {
		b.Config.CallbackPort = 29340
	}
	applyMediaDefaults(&b.Config.Media)

	b.Log = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("component", "bot").Logger()

	// Create WordPress client. If agent_token is set, use it as the default
	// (for onboarding/identity calls). Per-user tokens override this for messages.
	b.WP = wordpress.NewWordPressClient(
		b.Config.SiteURL,
		b.Config.AgentSlug,
		b.Config.AgentToken,
		b.Config.RequestTimeout,
	)
	b.Sessions = wordpress.NewSessionStore()

	// If a global agent token is set, validate it at startup.
	if b.Config.AgentToken != "" {
		ident, err := b.WP.GetIdentity(ctx)
		if err != nil {
			return fmt.Errorf("failed to validate agent token: %w", err)
		}
		if ident.Data != nil && ident.Data.AgentSlug != "" {
			b.WP.AgentSlug = ident.Data.AgentSlug
			b.Log.Info().
				Str("agent", ident.Data.AgentSlug).
				Str("site", ident.Data.SiteName).
				Msg("Agent identity confirmed (global token)")
		}
	} else {
		b.Log.Info().Msg("No global agent_token configured; per-user auth required")
	}

	// Initialize per-user auth database.
	authDBPath := b.Config.AuthDatabasePath
	if authDBPath == "" {
		// Default: same directory as the crypto database.
		authDBPath = filepath.Join(filepath.Dir(b.Config.DatabasePath), "bot-auth.db")
	}
	userAuth, err := NewUserAuth(authDBPath)
	if err != nil {
		return fmt.Errorf("failed to init user auth database: %w", err)
	}
	b.UserAuth = userAuth
	b.Log.Info().Str("path", authDBPath).Msg("User auth database initialized")

	// Start the PKCE callback server.
	if b.Config.CallbackURL != "" {
		b.Callback = newBotCallbackServer(b)
		if err := b.Callback.Start(ctx); err != nil {
			userAuth.Close()
			return fmt.Errorf("failed to start callback server: %w", err)
		}
	} else {
		b.Log.Warn().Msg("No callback_url configured; per-user PKCE auth will not work")
	}

	// Create mautrix client.
	userID := id.UserID(b.Config.UserID)
	client, err := mautrix.NewClient(b.Config.HomeserverURL, userID, b.Config.AccessToken)
	if err != nil {
		userAuth.Close()
		return fmt.Errorf("failed to create Matrix client: %w", err)
	}
	client.Log = b.Log.With().Str("component", "mautrix").Logger()
	b.Client = client

	// Set up E2EE via cryptohelper. The crypto helper handles login
	// internally — we always provide LoginAs credentials so it can
	// create/refresh device sessions as needed.
	pickleKey := []byte(b.Config.PickleKey)
	if len(pickleKey) == 0 {
		pickleKey = []byte("mautrix-datamachine-bot")
	}
	cryptoHelper, err := cryptohelper.NewCryptoHelper(client, pickleKey, b.Config.DatabasePath)
	if err != nil {
		userAuth.Close()
		return fmt.Errorf("failed to create crypto helper: %w", err)
	}

	// Always provide LoginAs so cryptohelper can manage the session.
	// If an access_token is configured, the client is already authenticated
	// but cryptohelper may still need to create a device for E2EE.
	if b.Config.Password != "" {
		cryptoHelper.LoginAs = &mautrix.ReqLogin{
			Type: mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{
				Type: mautrix.IdentifierTypeUser,
				User: b.Config.UserID,
			},
			Password:                 b.Config.Password,
			InitialDeviceDisplayName: "Data Machine Bot",
		}
	} else if b.Config.AccessToken != "" {
		// When using access_token only, cryptohelper needs the token set
		// on the client (already done above via NewClient). We also need
		// to resolve the device ID by calling /whoami.
		whoami, err := client.Whoami(ctx)
		if err != nil {
			userAuth.Close()
			return fmt.Errorf("failed to verify access token via /whoami: %w", err)
		}
		client.DeviceID = whoami.DeviceID
		b.Log.Info().
			Str("user_id", whoami.UserID.String()).
			Str("device_id", whoami.DeviceID.String()).
			Msg("Access token verified")
	} else {
		userAuth.Close()
		return fmt.Errorf("either access_token or password is required")
	}

	if err := cryptoHelper.Init(ctx); err != nil {
		userAuth.Close()
		return fmt.Errorf("failed to init crypto helper: %w", err)
	}
	b.CryptoHelper = cryptoHelper
	client.Crypto = cryptoHelper

	// Register event handlers.
	b.registerHandlers()

	b.Log.Info().Msg("Starting sync loop")

	// Run sync in a goroutine so we can select on context cancellation.
	syncErr := make(chan error, 1)
	go func() {
		syncErr <- client.SyncWithContext(ctx)
	}()

	select {
	case <-ctx.Done():
		b.Log.Info().Msg("Context cancelled, stopping bot")
		client.StopSync()
		_ = cryptoHelper.Close()
		_ = userAuth.Close()
		return nil
	case err := <-syncErr:
		_ = cryptoHelper.Close()
		_ = userAuth.Close()
		if err != nil {
			return fmt.Errorf("sync failed: %w", err)
		}
		return nil
	}
}
