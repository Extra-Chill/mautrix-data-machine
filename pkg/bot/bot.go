package bot

import (
	"context"
	"fmt"
	"os"
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
}

// Run starts the bot: logs in, sets up E2EE, registers handlers, and syncs.
// It blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if b.Config.HomeserverURL == "" || b.Config.UserID == "" {
		return fmt.Errorf("homeserver_url and user_id are required")
	}
	if b.Config.SiteURL == "" || b.Config.AgentToken == "" {
		return fmt.Errorf("site_url and agent_token are required")
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

	b.Log = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("component", "bot").Logger()

	// Create WordPress client.
	b.WP = wordpress.NewWordPressClient(
		b.Config.SiteURL,
		b.Config.AgentSlug,
		b.Config.AgentToken,
		b.Config.RequestTimeout,
	)
	b.Sessions = wordpress.NewSessionStore()

	// Validate agent token before starting the sync loop.
	ident, err := b.WP.GetIdentity(ctx)
	if err != nil {
		return fmt.Errorf("failed to validate agent token: %w", err)
	}
	if ident.Data != nil && ident.Data.AgentSlug != "" {
		b.WP.AgentSlug = ident.Data.AgentSlug
		b.Log.Info().
			Str("agent", ident.Data.AgentSlug).
			Str("site", ident.Data.SiteName).
			Msg("Agent identity confirmed")
	}

	// Create mautrix client.
	userID := id.UserID(b.Config.UserID)
	client, err := mautrix.NewClient(b.Config.HomeserverURL, userID, b.Config.AccessToken)
	if err != nil {
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
			return fmt.Errorf("failed to verify access token via /whoami: %w", err)
		}
		client.DeviceID = whoami.DeviceID
		b.Log.Info().
			Str("user_id", whoami.UserID.String()).
			Str("device_id", whoami.DeviceID.String()).
			Msg("Access token verified")
	} else {
		return fmt.Errorf("either access_token or password is required")
	}

	if err := cryptoHelper.Init(ctx); err != nil {
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
		return nil
	case err := <-syncErr:
		_ = cryptoHelper.Close()
		if err != nil {
			return fmt.Errorf("sync failed: %w", err)
		}
		return nil
	}
}
