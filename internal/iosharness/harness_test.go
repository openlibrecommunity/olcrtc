package iosharness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteServerConfigAppliesFreshRoomAndResolver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	template := filepath.Join(dir, "srv.yaml")
	output := filepath.Join(dir, "out.yaml")
	writeFile(t, template, `mode: srv
auth:
  provider: telemost
room:
  id: "https://telemost.yandex.ru/j/old-room"
  channel: old-channel
crypto:
  key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
net:
  transport: vp8channel
  dns: "1.1.1.1:53"
socks:
  host: "127.0.0.1"
  port: 1080
  proxy_addr: "127.0.0.1"
  proxy_port: 1081
  proxy_user: "old-user"
  proxy_pass: "old-pass"
`)

	sub := Subscription{
		Carrier:   "telemost",
		Room:      "https://telemost.yandex.ru/j/fresh-room",
		Channel:   "fresh-channel",
		CryptoKey: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Transport: "vp8channel",
	}
	if err := WriteServerConfig(template, output, sub, "8.8.8.8:53", true); err != nil {
		t.Fatalf("WriteServerConfig() error = %v", err)
	}

	got := readFile(t, output)
	for _, want := range []string{
		"provider: telemost",
		"id: https://telemost.yandex.ru/j/fresh-room",
		"channel: fresh-channel",
		"key: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"transport: vp8channel",
		"dns: 8.8.8.8:53",
		"proxy_addr: \"\"",
		"proxy_port: 0",
		"proxy_user: \"\"",
		"proxy_pass: \"\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("generated config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old-room") || strings.Contains(got, "old-pass") {
		t.Fatalf("generated config leaked stale room/proxy secret:\n%s", got)
	}
}

func TestSummarizeLogsGreenFreshRoomBulk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(raw, "app.log"), `2026-07-06 12:54:53 +0000 http probe loop start rounds=3 interval_s=8.0 download_bytes=1048576
2026-07-06 12:54:56 +0000 http probe ok label=ipify host=api.ipify.org status=200 bytes=22 duration_ms=2598
2026-07-06 12:54:58 +0000 http probe ok label=example host=example.com status=200 bytes=559 duration_ms=2632
2026-07-06 12:55:10 +0000 http probe ok label=download host=speed.cloudflare.com status=200 bytes=1048576 duration_ms=12255
2026-07-06 12:55:10 +0000 http probe round=1 done ok=3 fail=0
2026-07-06 12:55:27 +0000 http probe ok label=ipify host=api.ipify.org status=200 bytes=22 duration_ms=8160
2026-07-06 12:55:35 +0000 http probe ok label=example host=example.com status=200 bytes=559 duration_ms=7737
2026-07-06 12:55:49 +0000 http probe ok label=download host=speed.cloudflare.com status=200 bytes=1048576 duration_ms=13884
2026-07-06 12:55:49 +0000 http probe round=2 done ok=3 fail=0
2026-07-06 12:56:04 +0000 http probe ok label=ipify host=api.ipify.org status=200 bytes=22 duration_ms=6769
2026-07-06 12:56:11 +0000 http probe ok label=example host=example.com status=200 bytes=559 duration_ms=7423
2026-07-06 12:56:25 +0000 http probe ok label=download host=speed.cloudflare.com status=200 bytes=1048576 duration_ms=13702
2026-07-06 12:56:25 +0000 http probe round=3 done ok=3 fail=0
`)
	writeFile(t, filepath.Join(raw, "tunnel.log"), `2026-07-06 12:54:44 +0000 === startTunnel ===
2026-07-06 12:54:46 +0000 cnc session ready
2026-07-06 12:54:46 +0000 network settings applied (v4 default route, mapdns 198.18.0.2, v6 passthrough)
2026-07-06 12:54:46 +0000 SOCKS ready
2026-07-06 12:54:46 +0000 tun2socks starting mapdns=198.18.0.2 udp=disabled sessions=256 log=stderr/error
`)
	serverLog := filepath.Join(dir, "server.log")
	writeFile(t, serverLog, `2026/07/06 15:54:46 peer connected: device=80da0d66-1111-2222-3333-894108f1ca30 session=278121df-1111-2222-3333-1455d6da212e
2026/07/06 15:55:07 traffic: session=278121df-1111-2222-3333-1455d6da212e addr=speed.cloudflare.com:443 in=889 out=1056250
2026/07/06 15:55:45 traffic: session=278121df-1111-2222-3333-1455d6da212e addr=speed.cloudflare.com:443 in=889 out=1056140
2026/07/06 15:56:21 traffic: session=278121df-1111-2222-3333-1455d6da212e addr=speed.cloudflare.com:443 in=889 out=1056268
`)

	verdict, err := SummarizeLogs(RawLogPaths{
		IOSDir:    raw,
		ServerLog: serverLog,
	}, SummaryPaths{
		IOSDir:        filepath.Join(out, "ios"),
		ServerSummary: filepath.Join(out, "server-summary.log"),
	}, Criteria{Rounds: 3, DownloadBytes: 1048576})
	if err != nil {
		t.Fatalf("SummarizeLogs() error = %v", err)
	}
	if !verdict.Green {
		t.Fatalf("verdict.Green = false, reasons=%v", verdict.Reasons)
	}
	if verdict.DownloadOK != 3 || verdict.RoundOK != 3 || verdict.Reconnects != 0 {
		t.Fatalf("unexpected verdict stats: %+v", verdict)
	}

	serverSummary := readFile(t, filepath.Join(out, "server-summary.log"))
	if strings.Contains(serverSummary, "278121df") || strings.Contains(serverSummary, "80da0d66") {
		t.Fatalf("server summary was not sanitized:\n%s", serverSummary)
	}
}

func TestSummarizeLogsAcceptsCopiedAppGroupOlcDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(filepath.Join(raw, "olc"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(raw, "olc", "app.log"), `2026-07-06 12:54:53 +0000 http probe loop start rounds=1 interval_s=8.0 download_bytes=0
2026-07-06 12:55:10 +0000 http probe round=1 done ok=2 fail=0
`)
	writeFile(t, filepath.Join(raw, "olc", "tunnel.log"), `2026-07-06 12:54:44 +0000 === startTunnel ===
2026-07-06 12:54:46 +0000 cnc session ready
`)
	serverLog := filepath.Join(dir, "server.log")
	writeFile(t, serverLog, `2026/07/06 15:54:46 peer connected: device=80da0d66-1111-2222-3333-894108f1ca30 session=278121df-1111-2222-3333-1455d6da212e
`)

	verdict, err := SummarizeLogs(RawLogPaths{
		IOSDir:    raw,
		ServerLog: serverLog,
	}, SummaryPaths{
		IOSDir:        filepath.Join(out, "ios"),
		ServerSummary: filepath.Join(out, "server-summary.log"),
	}, Criteria{Rounds: 1, DownloadBytes: 0})
	if err != nil {
		t.Fatalf("SummarizeLogs() error = %v", err)
	}
	if !verdict.Green {
		t.Fatalf("verdict.Green = false, reasons=%v", verdict.Reasons)
	}
	if got := readFile(t, filepath.Join(out, "ios", "app-probes-summary.log")); !strings.Contains(got, "round=1 done ok=2 fail=0") {
		t.Fatalf("summary did not include app-group olc log:\n%s", got)
	}
}

func TestSummarizeLogsRedOnDownloadTimeoutAndReconnect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(raw, "app.log"), `2026-07-06 12:54:53 +0000 http probe loop start rounds=1 interval_s=8.0 download_bytes=1048576
2026-07-06 12:55:10 +0000 http probe error label=download host=speed.cloudflare.com duration_ms=60000 The request timed out.
2026-07-06 12:55:10 +0000 http probe round=1 done ok=2 fail=1
`)
	writeFile(t, filepath.Join(raw, "tunnel.log"), `2026-07-06 12:54:44 +0000 === startTunnel ===
2026-07-06 12:54:46 +0000 cnc session ready
`)
	serverLog := filepath.Join(dir, "server.log")
	writeFile(t, serverLog, `2026/07/06 15:55:07 traffic: session=278121df-1111-2222-3333-1455d6da212e addr=speed.cloudflare.com:443 in=889 out=1056250
2026/07/06 15:55:30 server reconnect reason=liveness - reinstalling smux session
`)

	verdict, err := SummarizeLogs(RawLogPaths{
		IOSDir:    raw,
		ServerLog: serverLog,
	}, SummaryPaths{
		IOSDir:        filepath.Join(out, "ios"),
		ServerSummary: filepath.Join(out, "server-summary.log"),
	}, Criteria{Rounds: 1, DownloadBytes: 1048576})
	if err != nil {
		t.Fatalf("SummarizeLogs() error = %v", err)
	}
	if verdict.Green {
		t.Fatalf("verdict.Green = true, want false")
	}
	joined := strings.Join(verdict.Reasons, "\n")
	for _, want := range []string{"download ok count 0 != expected rounds 1", "http probe errors: 1", "server reconnect/teardown events: 1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("verdict reasons missing %q: %v", want, verdict.Reasons)
		}
	}
}

func writeFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
