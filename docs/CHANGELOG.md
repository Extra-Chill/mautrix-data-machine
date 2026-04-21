# Changelog

## [0.6.0] - 2026-04-21

### Added
- forward Matrix `m.image` events through the bridge: download (E2EE-safe), upload to WP Media Library via `/wp/v2/media`, call `/bridge/send` with `attachments` so downstream socials chat tools (`publish_instagram` etc.) can consume public image URLs. (#12)
- `media:` config section (`enabled`, `max_bytes`, `allowed_mime_types`, `download_timeout`, `upload_timeout`) with sane defaults matching Instagram Graph API limits. (#12)
- `bridge_app` / `bridge_room` / `bridge_room_kind` context on `/bridge/send` so the agent's bridge-mode guidance can adapt per room. (#12, #13)
- shared `runChatTurn()` entry point so text and image paths converge on one session/tool-call loop. (#12)
- wire `bridge_app` / `bridge_room` / `bridge_room_kind` into `/bridge/send` (closes #10). (#13)
- forward Matrix m.image events as /bridge/send attachments

### Changed
- `WordPressClient.SendMessage()` signature now accepts `attachments []Attachment` and `*BridgeContext`. Only caller inside this repo (`pkg/bot/handlers.go`) updated; `pkg/connector` is unaffected.
- `registerHandlers()` now dispatches by `MsgType` (text/emote/notice → text path, image → new image path, others silently ignored) instead of gating only on empty body.

## [0.5.0] - 2026-04-06

### Added
- add per-user PKCE auth for bot mode
- add Matrix bot mode and extract shared WordPress API client
- add manual session reset command

### Changed
- fix gofmt alignment in pkce.go, login.go, poller.go

### Fixed
- use /whoami to resolve device ID when using access_token
- use /new as the session reset command
- limit reset command to /reset only

## [0.4.0] - 2026-04-04

### Added
- add typing indicators, session TTL rotation, and read receipts

## [0.3.0] - 2026-04-03

### Added
- create portal room on connect so user has a chat room
- verify agent access during login before completing
- skip site URL step when default_site_url is configured

### Fixed
- complete message delivery pipeline — encryption, localhost routing, continue loop, markdown rendering
- portal key mismatch, configurable branding, welcome message in portal room
- don't pre-generate session IDs, let WordPress create them
- let framework handle DM member list so user gets invited
- implement GetCapabilities and remove nil-returning duplicate
- provide RoomType and Members in ChatInfo to prevent portal creation panic
- uncomment callback_url/callback_port in example config

## [0.2.0] - 2026-04-03

### Added
- consume onboarding metadata and use bridge /send endpoint
- add browser-based PKCE login for Roadie

### Changed
- update API URLs from chat-bridge/v1 to datamachine/v1/bridge
- Scaffold mautrix-datamachine bridgev2 connector

### Fixed
- complete PKCE token exchange flow

## [0.1.0] - 2026-04-03

### Added
- Initial release: bridgev2 connector, PKCE browser login, onboarding metadata consumption, /send endpoint
