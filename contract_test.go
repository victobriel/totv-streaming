package main

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestAuth(t *testing.T) {
	e := newEnv(t, nil)
	for _, s := range []string{"", "wrong", testSecret + "x"} {
		code, m, raw := e.call("GET", "/v1/health", nil, s)
		if code != 401 || m["error"] != "unauthorized" {
			t.Fatalf("secret %q: %d %s", s, code, raw)
		}
	}
	if code, _, _ := e.call("GET", "/v1/health", nil, testSecret); code != 200 {
		t.Fatal(code)
	}
	// rotation: the file is re-read, no restart
	writeFile(t, e.srv.cfg.SecretFile, "new-secret")
	if code, _, _ := e.call("GET", "/v1/health", nil, testSecret); code != 401 {
		t.Fatal("old secret still accepted")
	}
	if code, _, _ := e.call("GET", "/v1/health", nil, "new-secret"); code != 200 {
		t.Fatal("rotated secret refused")
	}
}

func TestHealthContract(t *testing.T) {
	e := newEnv(t, nil)
	_, m, raw := e.call("GET", "/v1/health", nil, testSecret)
	for _, k := range []string{"version", "encoder", "gpu", "cores", "isolation"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("health without %q: %s", k, raw)
		}
	}
	iso := m["isolation"].(map[string]any)
	for _, k := range []string{"seccomp", "landlock", "netns"} {
		if _, ok := iso[k].(bool); !ok {
			t.Fatalf("isolation.%s not bool: %s", k, raw)
		}
	}
	// proved by the child itself (a real blocked socket()/ptrace()/open()): see probeIsolation
	if iso["seccomp"] != true || iso["landlock"] != true {
		t.Skipf("kernel without seccomp/landlock here: %s", raw)
	}
	if iso["netns"] != probeIsolation(testEmuBin()).Netns { // true where unprivileged user namespaces exist, false elsewhere
		t.Fatal("netns must match what the selftest proves")
	}
	if m["encoder"] != "x264" {
		t.Fatalf("encoder %v", m["encoder"])
	}
}

func TestSessionValidation(t *testing.T) {
	e := newEnv(t, nil)
	bad := func(mut func(map[string]any), want int) {
		t.Helper()
		b := e.createBody("v1")
		mut(b)
		if code, _, raw := e.call("POST", "/v1/sessions", b, testSecret); code != want {
			t.Fatalf("want %d got %d %s", want, code, raw)
		}
	}
	bad(func(b map[string]any) { b["serverUrl"] = "http://192.168.1.5:8080" }, 400)
	bad(func(b map[string]any) { b["serverUrl"] = "http://localhost:8080" }, 400)
	bad(func(b map[string]any) { b["serverUrl"] = "https://127.0.0.1:8080" }, 400)
	bad(func(b map[string]any) { b["platform"] = "n64" }, 400)
	bad(func(b map[string]any) { b["core"] = "../../etc/passwd" }, 400)
	bad(func(b map[string]any) { b["core"] = "mupen64plus_next" }, 400)
	bad(func(b map[string]any) { b["sessionId"] = "a/b" }, 400)
	bad(func(b map[string]any) { b["iceHost"] = "not-an-ip" }, 400)
	bad(func(b map[string]any) { b["mediaId"] = "../x" }, 400)
	// no redirect following: the ROM request gets a 302 and the session must fail
	e.totv.redirectIt = true
	bad(func(map[string]any) {}, 502)
	if n := len(e.srv.sessions); n != 0 {
		t.Fatalf("failed start left %d sessions", n)
	}
}

func TestSessionRefusedWithoutIsolation(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.iso = isolation{} // as if the kernel did not allow it
	if code, m, _ := e.call("POST", "/v1/sessions", e.createBody("x"), testSecret); code != 503 || m["error"] != "isolation_unavailable" {
		t.Fatalf("%d %v", code, m)
	}
}

// Full path: create -> signal(offer) -> media + input -> save on DELETE -> second session loads the save.
func TestSessionEndToEnd(t *testing.T) {
	e := newEnv(t, nil)
	e.create("s1")

	if code, m, _ := e.call("POST", "/v1/sessions", e.createBody("s2"), testSecret); code != 409 || m["error"] != "busy" {
		t.Fatalf("second session: %d %v", code, m)
	}
	code, _, raw := e.call("GET", "/v1/sessions", nil, testSecret)
	if code != 200 || string(raw) == "" {
		t.Fatal(code)
	}
	var list []map[string]any
	mustJSON(t, raw, &list)
	if len(list) != 1 || list[0]["id"] != "s1" || list[0]["state"] != "waiting_signal" {
		t.Fatalf("list: %s", raw)
	}
	if code, _, _ := e.call("POST", "/v1/sessions/nope/signal", map[string]string{"type": "offer", "sdp": "x"}, testSecret); code != 404 {
		t.Fatal("unknown session must be 404")
	}

	p := newTestPeer(t)
	defer p.pc.Close()
	p.negotiate(t, e, "s1")
	waitFor(t, "video+audio", 10*time.Second, func() bool { return p.vPackets.Load() > 30 && p.aPackets.Load() > 20 })
	if p.extID.Load() == 0 {
		t.Fatal("client did not offer playout-delay")
	}
	if p.pdOK.Load() == 0 || p.pdBad.Load() != 0 {
		t.Fatalf("playout-delay 0/0 on video packets: ok=%d bad=%d", p.pdOK.Load(), p.pdBad.Load())
	}

	// input: A pressed; an old/duplicate seq must not be applied
	p.sendInput(1 << 4)
	waitFor(t, "input applied", 3*time.Second, func() bool { return statI(t, e, "s1", "inputApplied") >= 1 })
	p.mu.Lock()
	old := encodeInput(inputPacket{seq: p.seq - 1, buttons: 0})
	p.mu.Unlock()
	p.dc.Send(old)
	p.dc.Send([]byte{1, 2, 3}) // malformed
	waitFor(t, "old dropped", 3*time.Second, func() bool { return statI(t, e, "s1", "inputOldDropped") >= 1 && statI(t, e, "s1", "inputBad") >= 1 })
	if statI(t, e, "s1", "inputApplied") != 1 {
		t.Fatal("stale packet was applied")
	}
	if statI(t, e, "s1", "playoutDelaySent") == 0 {
		t.Fatal("no playout-delay sent")
	}
	_, st, _ := e.call("GET", "/v1/sessions/s1", nil, testSecret)
	if st["state"] != "playing" {
		t.Fatalf("state %v", st["state"])
	}

	// stop: save is written to the (fake) Totv BEFORE the answer
	code, m, _ := e.call("DELETE", "/v1/sessions/s1", nil, testSecret)
	if code != 200 || m["ok"] != true {
		t.Fatalf("delete %d %v", code, m)
	}
	e.totv.mu.Lock()
	if e.totv.puts != 1 || len(e.totv.sram) != 8192 || binary.LittleEndian.Uint32(e.totv.sram) != 1 || e.totv.lastQuery != "core=server_testcore&mediaId=9" {
		t.Fatalf("save not written: puts=%d len=%d q=%s", e.totv.puts, len(e.totv.sram), e.totv.lastQuery)
	}
	e.totv.mu.Unlock()
	if code, _, _ := e.call("GET", "/v1/sessions/s1", nil, testSecret); code != 404 {
		t.Fatal("session still present")
	}

	// A9: the next session starts from the save (counter 1 -> 2)
	e.create("s3")
	if code, _, _ := e.call("DELETE", "/v1/sessions/s3", nil, testSecret); code != 200 {
		t.Fatal(code)
	}
	e.totv.mu.Lock()
	defer e.totv.mu.Unlock()
	if binary.LittleEndian.Uint32(e.totv.sram) != 2 || e.totv.puts != 2 {
		t.Fatalf("save not loaded by the next session: counter=%d puts=%d", binary.LittleEndian.Uint32(e.totv.sram), e.totv.puts)
	}
	if e.totv.badAuth != 0 {
		t.Fatal("component called the Totv without the token")
	}
}

func TestNoSignalTimeout(t *testing.T) {
	e := newEnv(t, func(c *config) { c.NoSignalTimeout = 3 * time.Second })
	e.create("idle")
	waitFor(t, "idle session ended", 12*time.Second, func() bool { return e.srv.get("idle") == nil })
}

func TestChildHasNoTokenAndNoEnv(t *testing.T) {
	e := newEnv(t, nil)
	e.create("c1")
	ss := e.srv.get("c1")
	if len(ss.cmd.Env) != 0 {
		t.Fatalf("child env: %v", ss.cmd.Env)
	}
	for _, a := range ss.cmd.Args {
		if a == "tok-abc" {
			t.Fatal("token passed to the child")
		}
	}
	if ss.flags&3 != 3 {
		t.Skip("kernel without full sandbox")
	}
}

func statI(t *testing.T, e *testEnv, id, key string) int64 {
	_, m, _ := e.call("GET", "/v1/sessions/"+id, nil, testSecret)
	st, _ := m["stats"].(map[string]any)
	f, _ := st[key].(float64)
	return int64(f)
}
