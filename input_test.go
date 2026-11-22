package main

import "testing"

func TestInputPacketRoundTrip(t *testing.T) {
	in := inputPacket{seq: 513, buttons: 1<<4 | 1<<14, axes: [4]int8{-127, 0, 5, 127}, port: 2}
	b := encodeInput(in)
	if len(b) != 12 || b[0] != 1 || b[1] != 2 || b[2] != 1 {
		t.Fatalf("bad encoding %v", b)
	}
	out, ok := parseInput(b)
	if !ok || out != in {
		t.Fatalf("round trip %v %v", ok, out)
	}
	for _, bad := range [][]byte{b[:11], append(append([]byte{}, b...), 0), append([]byte{2}, b[1:]...)} {
		if _, ok := parseInput(bad); ok {
			t.Fatal("accepted invalid packet")
		}
	}
	b[11] = 4
	if _, ok := parseInput(b); ok {
		t.Fatal("accepted port 4")
	}
}

func TestSeqWraparound(t *testing.T) {
	cases := []struct {
		seq, last uint16
		want      bool
	}{{2, 1, true}, {1, 1, false}, {0, 1, false}, {0, 65535, true}, {5, 65530, true}, {65530, 5, false}}
	for _, c := range cases {
		if got := newerSeq(c.seq, c.last); got != c.want {
			t.Errorf("newerSeq(%d,%d)=%v", c.seq, c.last, got)
		}
	}
}

func TestButtonMapping(t *testing.T) {
	// a (south) -> RetroPad B (bit 0); b (east) -> RetroPad A (bit 8); start -> bit 3; select -> bit 2; dpad_up -> bit 4
	if m := retroMask(1 << 4); m != 1<<0 {
		t.Fatalf("a: %b", m)
	}
	if m := retroMask(1 << 5); m != 1<<8 {
		t.Fatalf("b: %b", m)
	}
	if m := retroMask(1<<14 | 1<<15 | 1<<0 | 1<<16); m != 1<<3|1<<2|1<<4 {
		t.Fatalf("misc: %b", m)
	}
}
