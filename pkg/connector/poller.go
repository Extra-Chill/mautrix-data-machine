package connector

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

func (dmc *DataMachineClient) startPolling() {
	ctx, cancel := context.WithCancel(context.Background())
	dmc.pollCancel = cancel

	log := dmc.UserLogin.Log.With().Str("component", "poller").Logger()
	ctx = log.WithContext(ctx)

	pollInterval := dmc.Main.Config.PollInterval
	if pollInterval == 0 {
		pollInterval = 5 * time.Second
	}

	log.Info().Dur("interval", pollInterval).Msg("Starting poll loop")
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Poll loop stopped")
			return
		case <-ticker.C:
			dmc.pollOnce(ctx)
		}
	}
}

func (dmc *DataMachineClient) pollOnce(ctx context.Context) {
	log := zerolog.Ctx(ctx)

	messages, err := dmc.GetPendingMessages(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to poll for pending messages")
		return
	}

	if len(messages) == 0 {
		return
	}

	log.Debug().Int("count", len(messages)).Msg("Received pending messages")

	var ackIDs []string
	for _, msg := range messages {
		remoteEvent := &DataMachineRemoteMessage{
			portalKey: dmc.Main.ResolvePortalKey(msg.AgentSlug, dmc.UserLogin.ID),
			id:        networkid.MessageID(msg.QueueID),
			text:      msg.Content,
			agentSlug: msg.AgentSlug,
			timestamp: msg.Time(),
			sender:    EventSenderForAgent(msg.AgentSlug, dmc),
		}
		dmc.Main.br.QueueRemoteEvent(dmc.UserLogin, remoteEvent)
		ackIDs = append(ackIDs, msg.QueueID)
	}

	if len(ackIDs) > 0 {
		if err := dmc.AckPendingMessages(ctx, ackIDs); err != nil {
			log.Warn().Err(err).Msg("Failed to ack pending messages")
		}
	}
}

func EventSenderForAgent(agentSlug string, dmc *DataMachineClient) bridgev2.EventSender {
	return bridgev2.EventSender{
		IsFromMe:    false,
		SenderLogin: networkid.UserLoginID(hostFromURL(dmc.siteURL) + ":" + agentSlug),
		Sender:      networkid.UserID(agentSlug + "@" + hostFromURL(dmc.siteURL)),
	}
}

type DataMachineRemoteMessage struct {
	portalKey networkid.PortalKey
	id        networkid.MessageID
	text      string
	agentSlug string
	timestamp time.Time
	sender    bridgev2.EventSender
}

var _ bridgev2.RemoteMessage = (*DataMachineRemoteMessage)(nil)
var _ bridgev2.RemoteEventWithTimestamp = (*DataMachineRemoteMessage)(nil)

func (m *DataMachineRemoteMessage) GetType() bridgev2.RemoteEventType { return bridgev2.RemoteEventMessage }
func (m *DataMachineRemoteMessage) GetPortalKey() networkid.PortalKey { return m.portalKey }
func (m *DataMachineRemoteMessage) AddLogContext(c zerolog.Context) zerolog.Context {
	return c.Str("message_id", string(m.id)).Str("agent", m.agentSlug)
}
func (m *DataMachineRemoteMessage) GetSender() bridgev2.EventSender { return m.sender }
func (m *DataMachineRemoteMessage) GetID() networkid.MessageID       { return m.id }
func (m *DataMachineRemoteMessage) GetTimestamp() time.Time          { return m.timestamp }
func (m *DataMachineRemoteMessage) ConvertMessage(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI) (*bridgev2.ConvertedMessage, error) {
	return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{ID: "part0", Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: m.text}}}}, nil
}
