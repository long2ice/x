package openvpn

import "testing"

func TestReplayWindow(t *testing.T) {
	var w replayWindow

	if w.accept(0) {
		t.Fatal("packet id 0 must always be rejected")
	}
	if !w.accept(5) {
		t.Fatal("first id should be accepted")
	}
	if w.accept(5) {
		t.Fatal("exact duplicate must be rejected")
	}
	if !w.accept(6) {
		t.Fatal("next-in-order id should be accepted")
	}
	if !w.accept(3) {
		t.Fatal("out-of-order id within the window should be accepted")
	}
	if w.accept(3) {
		t.Fatal("duplicate of an in-window id must be rejected")
	}
	if !w.accept(200) {
		t.Fatal("large forward jump should be accepted")
	}
	if w.accept(6) {
		t.Fatal("id far behind the window must be rejected as stale")
	}
	if !w.accept(199) {
		t.Fatal("id just behind the new high should be accepted")
	}
	if w.accept(199) {
		t.Fatal("duplicate after a jump must be rejected")
	}
}
