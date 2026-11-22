package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const chromePage = `<!doctype html><meta charset=utf-8><body style="margin:0;background:#000;overflow:hidden"><video id=v autoplay muted playsinline style="width:640px;height:480px"></video>
<script>
window.st={frames:0,state:''};
(async()=>{
const pc=new RTCPeerConnection();
pc.addTransceiver('video',{direction:'recvonly'});pc.addTransceiver('audio',{direction:'recvonly'});
const dc=pc.createDataChannel('input',{ordered:false,maxRetransmits:0});
pc.ontrack=e=>{if(e.track.kind==='video'){document.getElementById('v').srcObject=new MediaStream([e.track]);}};
pc.onconnectionstatechange=()=>window.st.state=pc.connectionState;
let seq=0,n=0;
const n64=location.search.includes('n64');
dc.onopen=()=>setInterval(()=>{n++;seq=(seq+1)&0xffff;const b=new DataView(new ArrayBuffer(12));b.setUint8(0,1);b.setUint16(1,seq);
 b.setUint32(3,n64?(n%188<8?1<<14:0):(n%120<60?0:1<<(n/30%4|0))); /* N64: Start for 8 ticks every ~3 s */
 if(n64)b.setInt8(7,60*(((n/60|0)%2)*2-1)); /* left stick X wiggle */
 dc.send(b.buffer)},16);
const o=await pc.createOffer();await pc.setLocalDescription(o);
await new Promise(r=>{if(pc.iceGatheringState==='complete')return r();pc.onicegatheringstatechange=()=>pc.iceGatheringState==='complete'&&r();setTimeout(r,3000)});
const r=await fetch('/offer',{method:'POST',body:JSON.stringify({type:'offer',sdp:pc.localDescription.sdp})});
await pc.setRemoteDescription(await r.json());
setInterval(()=>{const v=document.getElementById('v');window.st.frames=v.getVideoPlaybackQuality?v.getVideoPlaybackQuality().totalVideoFrames:0;window.st.t=v.currentTime},500);
})();
</script>`

// TestChromeServe (SNES_CHROME=<seconds> or N64_CHROME=<seconds>): serves a page for a real Chrome, relaying signaling to the
// component. N64 uses mupen64plus-next on the GPU (ROM: N64_ROM, a file outside the repo); the page is then opened
// with ?n64 (Start every 3 s, stick wiggle). scripts/chrome-shot.mjs drives Chrome headless and takes the PNG.
func TestChromeServe(t *testing.T) {
	secs := 0
	n64 := false
	fmt.Sscan(os.Getenv("SNES_CHROME"), &secs)
	if secs == 0 {
		fmt.Sscan(os.Getenv("N64_CHROME"), &secs)
		n64 = secs > 0
	}
	psp := false
	if secs == 0 {
		fmt.Sscan(os.Getenv("PSP_CHROME"), &secs)
		psp = secs > 0
	}
	if secs == 0 {
		t.Skip("set SNES_CHROME=<seconds>, N64_CHROME=<seconds> or PSP_CHROME=<seconds> (PSP=1 PSP_ROM=...)")
	}
	romPath := os.Getenv("SNES_ROM") // ROMs live outside the repository: no default path
	if n64 {
		romPath = os.Getenv("N64_ROM")
	}
	if romPath == "" && !psp {
		t.Skip("set SNES_ROM=<file> (or N64_ROM=<file> with N64_CHROME)")
	}
	if psp {
		e, _ := pspRealEnv(t)
		runChrome(t, e, pspBody(e, "snes"), secs, os.Getenv("PSP_CHROME_URLFILE"), "/?n64")
		return
	}
	rom, err := os.ReadFile(romPath)
	if err != nil {
		t.Skip(err)
	}
	cwd, _ := os.Getwd()
	e := newEnv(t, func(c *config) { c.CoresDir = filepath.Join(cwd, "cores"); c.DevTestCore = false })
	e.totv.rom = rom
	body := e.createBody("snes")
	body["core"] = "server_snes9x"
	if n64 {
		skipWithoutGPU(t, e)
		body["platform"], body["core"] = "n64", "server_mupen64plus_next"
	}
	urlFile, suffix := os.Getenv("SNES_CHROME_URLFILE"), ""
	if n64 {
		urlFile, suffix = os.Getenv("N64_CHROME_URLFILE"), "/?n64"
	}
	runChrome(t, e, body, secs, urlFile, suffix)
}

// runChrome creates the session `body` describes, serves the offer/answer page for it and waits `secs` seconds while chrome-shot.mjs looks at it.
func runChrome(t *testing.T, e *testEnv, body map[string]any, secs int, urlFile, suffix string) {
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/offer" {
			b, _ := io.ReadAll(r.Body)
			req, _ := http.NewRequest("POST", "http://unix/v1/sessions/snes/signal", bytesReader(b))
			req.Header.Set("Authorization", "Bearer "+testSecret)
			res, err := e.client.Do(req)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer res.Body.Close()
			io.Copy(w, res.Body)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, chromePage)
	}))
	defer web.Close()
	os.WriteFile(urlFile, []byte(web.URL+suffix), 0o644)
	for i := 0; i < secs; i++ {
		time.Sleep(time.Second)
		_, st, _ := e.call("GET", "/v1/sessions/snes", nil, testSecret)
		s, _ := st["stats"].(map[string]any)
		t.Logf("t=%ds state=%v framesIn=%v enc=%v audio=%v inRecv=%v", i, st["state"], s["framesIn"], s["framesEncoded"], s["audioPackets"], s["inputRecv"])
	}
}

func bytesReader(b []byte) io.Reader { return &byteRd{b: b} }

type byteRd struct{ b []byte }

func (r *byteRd) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
