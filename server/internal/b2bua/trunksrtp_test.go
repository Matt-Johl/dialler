package b2bua

import (
	"errors"
	"testing"
)

func TestParseTrunkSRTP(t *testing.T) {
	for in, want := range map[string]TrunkSRTPMode{
		"": TrunkSRTPOff, "off": TrunkSRTPOff, "OFF": TrunkSRTPOff,
		" sdes ": TrunkSRTPSDES, "SDES": TrunkSRTPSDES,
	} {
		got, err := ParseTrunkSRTP(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	// "offer"/"require" were an earlier three-mode design, dropped because
	// the two secure modes differed only for a malformed peer (SPEC §6
	// item 3a). Refused rather than quietly aliased.
	for _, bad := range []string{"yes", "1", "offer", "require", "optimistic"} {
		if _, err := ParseTrunkSRTP(bad); err == nil {
			t.Errorf("%q should be refused rather than guessed at", bad)
		}
	}
}

// Only "sdes" puts a=crypto on the trunk leg. Getting this
// wrong in either direction is a call that fails: a PBX without encryption
// answers an RTP/SAVP offer with 488.
func TestOnlyTheSecureModesOfferCrypto(t *testing.T) {
	for mode, want := range map[TrunkSRTPMode]int{
		TrunkSRTPOff: 0, TrunkSRTPSDES: 1,
	} {
		if got := srtpOption(mode); got != want {
			t.Errorf("%q: diago MediaSRTP %d, want %d", mode, got, want)
		}
	}
}

// sdes is the only mode that refuses a leg; and it only ever judges the
// TRUNK leg, because the app leg is unconditionally SRTP (SPEC §4.4 rule 4)
// and has no mode to consult.
func TestOnlySdesJudgesAndOnlyTheTrunkLeg(t *testing.T) {
	insecure := func(s *Server, trunk bool) error { return s.requireTrunkSRTP(trunk, nil) }

	off := &Server{cfg: Config{TrunkSRTP: TrunkSRTPOff}}
	if err := insecure(off, true); err != nil {
		t.Errorf("off: an unencrypted trunk leg should be allowed, got %v", err)
	}

	s := &Server{cfg: Config{TrunkSRTP: TrunkSRTPSDES}}
	if err := insecure(s, false); err != nil {
		t.Errorf("the app leg must not be judged by the trunk mode: %v", err)
	}
	if err := insecure(s, true); !errors.Is(err, errTrunkNotSecure) {
		t.Errorf("an unencrypted trunk leg under sdes should fail, got %v", err)
	}
}
