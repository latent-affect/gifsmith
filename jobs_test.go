package main

import "testing"

// TestEncodeTimeoutBounds is the fix for a real finding: encode jobs had
// no wall-clock timeout at all (context.WithCancel, never WithTimeout),
// unlike TranscribeManager's already-timed-out jobs — a pathologically
// slow/huge input could hold the single encode semaphore indefinitely,
// blocking every other queued job. This checks the bounding function's
// shape rather than waiting out a real multi-minute timeout in a test.
func TestEncodeTimeoutBounds(t *testing.T) {
	if got := encodeTimeout(0); got != encodeBaseTimeout {
		t.Errorf("zero-duration job: got %v, want base %v", got, encodeBaseTimeout)
	}
	short := encodeTimeout(10)
	if short <= encodeBaseTimeout {
		t.Errorf("10s job: timeout %v should exceed base %v", short, encodeBaseTimeout)
	}
	if got := encodeTimeout(100000); got != encodeMaxTimeout {
		t.Errorf("huge duration: got %v, want capped at %v", got, encodeMaxTimeout)
	}
}
