package ws

import (
	"testing"
)

func TestPresenceTracker(t *testing.T) {
	tracker := NewPresenceTracker()

	// Initial check
	if tracker.IsOnline("user-1") {
		t.Errorf("expected user-1 to be offline initially")
	}

	// Connect 1st session
	justOnline := tracker.Connect("user-1")
	if !justOnline {
		t.Errorf("expected justOnline to be true on first connect")
	}
	if !tracker.IsOnline("user-1") {
		t.Errorf("expected user-1 to be online")
	}

	// Connect 2nd session (e.g. user opens second tab or another socket)
	justOnline2 := tracker.Connect("user-1")
	if justOnline2 {
		t.Errorf("expected justOnline to be false on second connect")
	}
	if !tracker.IsOnline("user-1") {
		t.Errorf("expected user-1 to still be online")
	}

	// Connect user-2
	tracker.Connect("user-2")
	online := tracker.OnlineUserIDs()
	if len(online) != 2 {
		t.Errorf("expected 2 online users, got %d", len(online))
	}

	// Disconnect 1 session of user-1
	justOffline := tracker.Disconnect("user-1")
	if justOffline {
		t.Errorf("expected justOffline to be false while 1 session remains")
	}
	if !tracker.IsOnline("user-1") {
		t.Errorf("expected user-1 to remain online with 1 active session")
	}

	// Disconnect final session of user-1
	justOffline2 := tracker.Disconnect("user-1")
	if !justOffline2 {
		t.Errorf("expected justOffline to be true when last session disconnects")
	}
	if tracker.IsOnline("user-1") {
		t.Errorf("expected user-1 to be offline now")
	}

	// Disconnect unknown user
	if tracker.Disconnect("non-existent") {
		t.Errorf("expected false when disconnecting unknown user")
	}
}
