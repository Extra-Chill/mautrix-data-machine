package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"
)

func TestDeriveBridgeContextFromPortal_Nil(t *testing.T) {
	if got := deriveBridgeContextFromPortal(nil); got != nil {
		t.Fatalf("nil portal should produce nil BridgeContext, got %+v", got)
	}
}

func TestDeriveBridgeContextFromPortal_DM(t *testing.T) {
	portal := &bridgev2.Portal{
		Portal: &database.Portal{
			MXID:     id.RoomID("!dm:example.com"),
			RoomType: database.RoomTypeDM,
		},
	}
	got := deriveBridgeContextFromPortal(portal)
	if got == nil {
		t.Fatal("expected non-nil BridgeContext")
	}
	if got.App != "matrix" {
		t.Errorf("App: want matrix, got %q", got.App)
	}
	if got.Room != "!dm:example.com" {
		t.Errorf("Room: want !dm:example.com, got %q", got.Room)
	}
	if got.RoomKind != "dm" {
		t.Errorf("RoomKind: want dm, got %q", got.RoomKind)
	}
}

func TestDeriveBridgeContextFromPortal_GroupDM(t *testing.T) {
	portal := &bridgev2.Portal{
		Portal: &database.Portal{
			MXID:     id.RoomID("!gdm:example.com"),
			RoomType: database.RoomTypeGroupDM,
		},
	}
	got := deriveBridgeContextFromPortal(portal)
	if got.RoomKind != "dm" {
		t.Errorf("GroupDM should classify as dm, got %q", got.RoomKind)
	}
}

func TestDeriveBridgeContextFromPortal_Group(t *testing.T) {
	portal := &bridgev2.Portal{
		Portal: &database.Portal{
			MXID:     id.RoomID("!group:example.com"),
			RoomType: database.RoomTypeDefault,
		},
	}
	got := deriveBridgeContextFromPortal(portal)
	if got.RoomKind != "group" {
		t.Errorf("Default room type should classify as group, got %q", got.RoomKind)
	}
}

func TestDeriveBridgeContextFromPortal_Space(t *testing.T) {
	portal := &bridgev2.Portal{
		Portal: &database.Portal{
			MXID:     id.RoomID("!space:example.com"),
			RoomType: database.RoomTypeSpace,
		},
	}
	got := deriveBridgeContextFromPortal(portal)
	if got.RoomKind != "group" {
		t.Errorf("Space should classify as group (conservative), got %q", got.RoomKind)
	}
}

func TestDeriveBridgeContextFromPortal_AppAlwaysMatrix(t *testing.T) {
	// The connector transport is Matrix regardless of room type. Upstream
	// network detection (via Beeper ghost-user MXIDs) is future work.
	portal := &bridgev2.Portal{
		Portal: &database.Portal{
			MXID:     id.RoomID("!x:example.com"),
			RoomType: database.RoomTypeDM,
		},
	}
	got := deriveBridgeContextFromPortal(portal)
	if got.App != "matrix" {
		t.Errorf("App should always be matrix for v1, got %q", got.App)
	}
}
