package main

import "encoding/binary"

// Input packet (12 bytes, big-endian), proposal documented in README.
//
//	0      version  uint8 (=1)
//	1-2    seq      uint16
//	3-6    buttons  uint32 bitmask (bit i = i-th name of buttonNames, LSB = bit 0)
//	7-10   axes     4 x int8: left X, left Y, right X, right Y
//	11     port     uint8 0..3
const inputLen = 12

var buttonNames = []string{"dpad_up", "dpad_down", "dpad_left", "dpad_right", "a", "b", "x", "y",
	"l1", "r1", "l2", "r2", "l3", "r3", "start", "select", "guide"}

// bit of the packet -> bit of the libretro RetroPad mask (-1: no equivalent).
// a/b/x/y name the PHYSICAL position on an Xbox-style pad (a=south, b=east, x=west, y=north), as Android's
// KEYCODE_BUTTON_A/B/X/Y do; RetroPad names are by SNES label (B=south, A=east, Y=west, X=north).
var padToRetro = [32]int{
	0: 4, 1: 5, 2: 6, 3: 7, // dpad up/down/left/right
	4: 0, 5: 8, 6: 1, 7: 9, // a->B, b->A, x->Y, y->X
	8: 10, 9: 11, 10: 12, 11: 13, 12: 14, 13: 15, // l1 r1 l2 r2 l3 r3
	14: 3, 15: 2, // start, select
	16: -1, // guide
}

type inputPacket struct {
	seq     uint16
	buttons uint32
	axes    [4]int8
	port    uint8
}

func parseInput(b []byte) (inputPacket, bool) {
	var p inputPacket
	if len(b) != inputLen || b[0] != 1 || b[11] > 3 {
		return p, false
	}
	p.seq = binary.BigEndian.Uint16(b[1:3])
	p.buttons = binary.BigEndian.Uint32(b[3:7])
	for i := 0; i < 4; i++ {
		p.axes[i] = int8(b[7+i])
	}
	p.port = b[11]
	return p, true
}

func encodeInput(p inputPacket) []byte {
	b := make([]byte, inputLen)
	b[0] = 1
	binary.BigEndian.PutUint16(b[1:3], p.seq)
	binary.BigEndian.PutUint32(b[3:7], p.buttons)
	for i := 0; i < 4; i++ {
		b[7+i] = byte(p.axes[i])
	}
	b[11] = p.port
	return b
}

func retroMask(buttons uint32) uint16 {
	var m uint16
	for i := 0; i < 17; i++ {
		if buttons&(1<<uint(i)) != 0 && padToRetro[i] >= 0 {
			m |= 1 << uint(padToRetro[i])
		}
	}
	return m
}

// newerSeq: 16-bit wrap-around comparison; true when seq is strictly newer than last.
func newerSeq(seq, last uint16) bool { return int16(seq-last) > 0 }

// N64 (mupen64plus-next with alt-map=True, "independent C buttons"). RetroPad names are by SNES label (B=south, A=east,
// Y=west, X=north); the core maps: B->N64 A, Y->N64 B, A->C-Down, X->C-Up, L->C-Left, R->C-Right, L2->Z, R2->R, SELECT->L.
// The packet bit -> RetroPad bit table below makes a DualShock/Xbox pad feel right: cross/A=N64 A, square/X=N64 B,
// circle/B=C-Down, triangle/Y=C-Up, L1=L, R1 and R2=R, L2=Z, L3=C-Left, R3=C-Right, Start. The right stick also drives the
// C buttons inside the core (axes 2 and 3 of the packet).
var padToRetroN64 = [32]int{
	0: 4, 1: 5, 2: 6, 3: 7, // dpad
	4:  0,  // a (south)   -> RetroPad B -> N64 A
	5:  8,  // b (east)    -> RetroPad A -> C-Down
	6:  1,  // x (west)    -> RetroPad Y -> N64 B
	7:  9,  // y (north)   -> RetroPad X -> C-Up
	8:  2,  // l1          -> RetroPad SELECT -> N64 L
	9:  13, // r1          -> RetroPad R2 -> N64 R
	10: 12, // l2          -> RetroPad L2 -> Z
	11: 13, // r2          -> RetroPad R2 -> N64 R
	12: 10, // l3          -> RetroPad L -> C-Left
	13: 11, // r3          -> RetroPad R -> C-Right
	14: 3,  // start
	15: -1, 16: -1,
}

// retroMaskFor converts the packet's buttons with the table of the platform ("n64" or the SNES one).
func retroMaskFor(platform string, buttons uint32) uint16 {
	var table *[32]int
	switch platform {
	case "n64":
		table = &padToRetroN64
	case "psp":
		table = &padToRetroPSP
	default:
		return retroMask(buttons)
	}
	var m uint16
	for i := 0; i < 32; i++ {
		if buttons&(1<<i) != 0 && table[i] >= 0 {
			m |= 1 << table[i]
		}
	}
	return m
}

// hasAnalog: platforms whose core reads the analog stick(s) of the packet (the SNES has none).
func hasAnalog(platform string) bool { return platform == "n64" || platform == "psp" }

// PSP (PPSSPP libretro): RetroPad B = Cross, A = Circle, Y = Square, X = Triangle, L/R = L/R, Start/Select, D-pad (libretro.cpp of PPSSPP).
// That is the SNES table (a = south = Cross, b = east = Circle, x = west = Square, y = north = Triangle: the PSP's face buttons have the same
// physical layout as an Xbox/DualShock pad), plus L2/R2 as extra L/R for pads whose shoulders are triggers. The PSP has ONE analog stick
// (the left stick of the packet); the right stick and l3/r3 have no PSP equivalent and are ignored (the core reads the right stick, no
// PSP game does).
var padToRetroPSP = [32]int{
	0: 4, 1: 5, 2: 6, 3: 7,
	4: 0, 5: 8, 6: 1, 7: 9,
	8: 10, 9: 11, 10: 10, 11: 11, // l1/l2 -> L, r1/r2 -> R
	12: -1, 13: -1, 14: 3, 15: 2, 16: -1,
}
