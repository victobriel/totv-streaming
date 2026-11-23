// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"reflect"
	"testing"
)

func TestN64VideoOptions(t *testing.T) {
	def := []string{"v.mupen64plus-EnableOverscan=Enabled", "v.mupen64plus-OverscanLeft=19", "v.mupen64plus-OverscanRight=14", "v.mupen64plus-OverscanTop=11", "v.mupen64plus-OverscanBottom=14"}
	for _, in := range []string{"", "garbage", "1,2,3", "1,2,3,-4", "1,2,3,99", "a,b,c,d"} {
		if got := n64VideoOptions(in, ""); !reflect.DeepEqual(got, def) {
			t.Errorf("%q: %v", in, got)
		}
	}
	if got := n64VideoOptions(" 1, 2,3 ,4", ""); got[1] != "v.mupen64plus-OverscanLeft=1" || got[4] != "v.mupen64plus-OverscanBottom=4" {
		t.Errorf("custom: %v", got)
	}
	if got := n64VideoOptions("off", ""); !reflect.DeepEqual(got, []string{"v.mupen64plus-EnableOverscan=Disabled"}) {
		t.Errorf("off: %v", got)
	}
	if got := n64VideoOptions("", "16:9"); got[len(got)-1] != "v.mupen64plus-aspect=16:9" {
		t.Errorf("16:9: %v", got)
	}
	if got := n64VideoOptions("", "stretch"); len(got) != 5 {
		t.Errorf("unknown aspect must stay 4:3: %v", got)
	}
}

// the crop must remove the border and keep 4:3: a synthetic picture with a black frame, same arithmetic the core applies (native 320x240 -> x2)
func TestContentBBox(t *testing.T) {
	w, h := 640, 480
	b := make([]byte, 3*w*h)
	for y := 23; y < 451; y++ {
		for x := 38; x < 613; x++ {
			b[3*(y*w+x)] = 200
		}
	}
	if got := contentBBox(b, w, h, 24); got != [4]int{38, 23, 613, 451} {
		t.Errorf("bbox %v", got)
	}
	if contentBBox(make([]byte, 3*w*h), w, h, 24) != [4]int{} {
		t.Error("black picture must give an empty box")
	}
	// remaining area after the default crop is 4:3 within 1%
	rw, rh := float64(320-19-14), float64(240-11-14)
	if r := rw / rh / (4.0 / 3); r < 0.99 || r > 1.01 {
		t.Errorf("default crop aspect off: %.4f", r)
	}
}
