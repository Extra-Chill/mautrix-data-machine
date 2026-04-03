# Changelog

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
