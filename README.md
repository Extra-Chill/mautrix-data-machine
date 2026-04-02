# mautrix-data-machine

Matrix / Beeper bridge connector for Data Machine.

This repo is the **chat client side** of the integration. It connects the Matrix / Beeper bridge ecosystem to Data Machine’s generic WordPress bridge endpoints.

## What this does

- provides a Beeper-friendly login flow for Data Machine agents
- uses browser-based PKCE auth instead of raw bearer token copy/paste
- registers a callback/webhook with WordPress
- maps Beeper rooms to Data Machine chat sessions
- polls and accepts token-scoped outbound responses

## Architecture

```text
Beeper user
   ↓
mautrix-data-machine
   ↓
data-machine-chat-bridge
   ↓
Data Machine core
   ↓
Agent (Roadie, Sarai, etc.)
```

## New user experience

If you are a new user adding this to Beeper, here is what should happen.

### 1. Add the bridge in Beeper

You begin the Data Machine login flow inside Beeper.

### 2. Beeper shows a QR code or login link

The bridge gives you a browser login step.

### 3. Open the WordPress approval screen

You are sent to the Extra Chill site where the target agent lives, for example:

- https://studio.extrachill.com

### 4. Log in to WordPress if needed

If you are not already signed in, WordPress asks you to authenticate normally.

### 5. Approve the agent

You see an approval screen for the target agent — for example **Roadie** — and click authorize.

### 6. Return to Beeper automatically

WordPress redirects back to the bridge callback URL.

The bridge exchanges the short-lived auth code for a Data Machine bearer token and completes the login.

### 7. Start chatting

Once the login is complete, your Beeper room becomes the live chat surface for the agent.

By default:

```text
one Beeper room
→ one bridge login
→ one active Data Machine session
```

Future UX can add:

- `/new`
- `/reset`
- session switching inside a room

## Why PKCE login matters

This flow is designed for non-technical users.

The user never has to:

- inspect bearer tokens
- copy secrets manually
- paste API credentials into Beeper

Instead, the bridge handles token exchange after browser approval.

## Token/login routing

The bridge expects WordPress to route by:

```text
agent_id + token_id
```

This means multiple users or devices can talk to the same agent without receiving each other’s responses.

## Configuration

See:

- `pkg/connector/example-config.yaml`

Important settings include:

- `default_site_url`
- `agent_slug`
- `callback_url`
- `callback_port`

## Build

### Native

```bash
./build.sh
```

### Test

```bash
go test ./...
```

### Requirements

- Go
- `libolm-dev`

## Homeboy

This repo is registered as a Homeboy component.

- component id: `mautrix-data-machine`
- extension: `go`
- version target: `component.json`

## Operator notes

- this repo depends on `data-machine-chat-bridge` being installed on the target WordPress site
- the callback URL used here must be allowed by the target agent’s redirect allowlist
- agent-specific access policy belongs in the platform plugin (for Extra Chill, that is `extrachill-roadie` for Roadie)
