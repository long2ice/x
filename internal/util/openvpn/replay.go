package openvpn

import "sync"

// ReplayWindow is a 64-slot sliding bitmap for rejecting duplicate or
// excessively old packet IDs on the receive path. Safe for concurrent
// callers.
type ReplayWindow struct {
	mu   sync.Mutex
	high uint32
	bits uint64
	set  bool
}

func NewReplayWindow() *ReplayWindow { return &ReplayWindow{} }

// Accept reports whether the given id has not been seen and is within the
// recent-64 window. id == 0 is rejected as a sentinel.
func (w *ReplayWindow) Accept(id uint32) bool {
	if id == 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.set {
		w.set = true
		w.high = id
		w.bits = 1
		return true
	}
	if id > w.high {
		shift := id - w.high
		if shift >= 64 {
			w.bits = 1
		} else {
			w.bits = (w.bits << shift) | 1
		}
		w.high = id
		return true
	}
	diff := w.high - id
	if diff >= 64 {
		return false
	}
	mask := uint64(1) << diff
	if w.bits&mask != 0 {
		return false
	}
	w.bits |= mask
	return true
}
