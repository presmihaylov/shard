package main

import "testing"

// e2e_test.sh pins seen to the same digest, so the echo and the host that checks it agree on one form.
func TestDigestIsWhatSha256sumPrints(t *testing.T) {
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := digest("abc"); got != want {
		t.Fatalf("digest(abc) = %s, want %s", got, want)
	}
}
