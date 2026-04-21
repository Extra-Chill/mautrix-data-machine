package bot

import (
	"context"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestDeriveBridgeContext_NilEvent(t *testing.T) {
	b := &Bot{}
	if got := b.deriveBridgeContext(context.Background(), nil); got != nil {
		t.Fatalf("nil event should produce nil BridgeContext, got %+v", got)
	}
}

func TestDeriveRoomKind_CachedDM(t *testing.T) {
	b := &Bot{}
	roomID := id.RoomID("!dm:example.com")
	b.roomKindCache.Store(roomID, "dm")

	got := b.deriveRoomKind(context.Background(), roomID)
	if got != "dm" {
		t.Errorf("cached dm should return dm, got %q", got)
	}
}

func TestDeriveRoomKind_CachedGroup(t *testing.T) {
	b := &Bot{}
	roomID := id.RoomID("!group:example.com")
	b.roomKindCache.Store(roomID, "group")

	got := b.deriveRoomKind(context.Background(), roomID)
	if got != "group" {
		t.Errorf("cached group should return group, got %q", got)
	}
}

func TestDeriveRoomKind_EmptyRoomID(t *testing.T) {
	b := &Bot{}
	// Empty room ID short-circuits to dm without hitting the network.
	got := b.deriveRoomKind(context.Background(), id.RoomID(""))
	if got != "dm" {
		t.Errorf("empty room ID should default to dm, got %q", got)
	}
}

func TestDeriveBridgeContext_UsesEventRoomID(t *testing.T) {
	b := &Bot{}
	roomID := id.RoomID("!r:example.com")
	// Prime the cache so the network call is skipped.
	b.roomKindCache.Store(roomID, "dm")

	evt := &event.Event{RoomID: roomID}
	got := b.deriveBridgeContext(context.Background(), evt)
	if got == nil {
		t.Fatal("expected non-nil BridgeContext")
	}
	if got.App != "matrix" {
		t.Errorf("App: want matrix, got %q", got.App)
	}
	if got.Room != roomID.String() {
		t.Errorf("Room: want %s, got %q", roomID, got.Room)
	}
	if got.RoomKind != "dm" {
		t.Errorf("RoomKind: want dm, got %q", got.RoomKind)
	}
}
