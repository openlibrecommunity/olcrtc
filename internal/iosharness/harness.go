// Package iosharness contains the reusable pieces for the local iOS Telemost
// verification harness: fresh-room config rendering, sanitized log extraction,
// and green/red verdict classification.
package iosharness

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Subscription is the compact Telemost subscription shared by srv and iOS cnc.
type Subscription struct {
	Carrier   string `json:"carrier"`
	Room      string `json:"room"`
	Channel   string `json:"channel"`
	CryptoKey string `json:"crypto_key"`
	Transport string `json:"transport"`
}

// Criteria describes the expected probe result.
type Criteria struct {
	Rounds        int
	DownloadBytes int64
}

// RawLogPaths points at raw logs. These paths may contain sensitive data and
// should stay under .secrets/runtime.
type RawLogPaths struct {
	IOSDir    string
	ServerLog string
}

// SummaryPaths points at sanitized output logs that are safe to keep in
// artifacts.
type SummaryPaths struct {
	IOSDir        string
	ServerSummary string
}

// Verdict is a machine-readable green/red result for a probe run.
type Verdict struct {
	Green      bool
	Reasons    []string
	RoundOK    int
	DownloadOK int
	HTTPError  int
	Reconnects int
}

// WriteServerConfig writes a server config from templatePath, replacing only
// room/subscription fields and local-test DNS/proxy settings.
func WriteServerConfig(templatePath, outputPath string, sub Subscription, resolver string, directExit bool) error {
	if err := validateSubscription(sub); err != nil {
		return err
	}
	data, err := os.ReadFile(templatePath) // #nosec G304 -- explicit local harness path
	if err != nil {
		return fmt.Errorf("read server config template: %w", err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse server config template: %w", err)
	}

	setNested(cfg, "auth", "provider", sub.Carrier)
	setNested(cfg, "room", "id", sub.Room)
	setNested(cfg, "room", "channel", sub.Channel)
	setNested(cfg, "crypto", "key", sub.CryptoKey)
	setNested(cfg, "net", "transport", sub.Transport)
	if resolver != "" {
		setNested(cfg, "net", "dns", resolver)
	}
	if directExit {
		setNested(cfg, "socks", "proxy_addr", "")
		setNested(cfg, "socks", "proxy_port", 0)
		setNested(cfg, "socks", "proxy_user", "")
		setNested(cfg, "socks", "proxy_pass", "")
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal server config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return fmt.Errorf("create server config dir: %w", err)
	}
	if err := os.WriteFile(outputPath, out, 0o600); err != nil {
		return fmt.Errorf("write server config: %w", err)
	}
	return nil
}

func validateSubscription(sub Subscription) error {
	var missing []string
	if sub.Carrier == "" {
		missing = append(missing, "carrier")
	}
	if sub.Room == "" {
		missing = append(missing, "room")
	}
	if sub.Channel == "" {
		missing = append(missing, "channel")
	}
	if sub.CryptoKey == "" {
		missing = append(missing, "crypto_key")
	}
	if sub.Transport == "" {
		missing = append(missing, "transport")
	}
	if len(missing) > 0 {
		return fmt.Errorf("subscription missing fields: %s", strings.Join(missing, ", "))
	}
	if _, err := url.ParseRequestURI(sub.Room); err != nil {
		return fmt.Errorf("subscription room is not a URL: %w", err)
	}
	if !regexp.MustCompile(`\A[0-9a-fA-F]{64}\z`).MatchString(sub.CryptoKey) {
		return errors.New("subscription crypto_key must be 64 hex chars")
	}
	return nil
}

func setNested(root map[string]any, section, key string, value any) {
	child, ok := root[section].(map[string]any)
	if !ok {
		child = map[string]any{}
		root[section] = child
	}
	child[key] = value
}

// SummarizeLogs writes sanitized summaries and returns the green/red verdict.
func SummarizeLogs(raw RawLogPaths, out SummaryPaths, criteria Criteria) (Verdict, error) {
	if criteria.Rounds <= 0 {
		return Verdict{}, errors.New("criteria rounds must be positive")
	}
	if criteria.DownloadBytes < 0 {
		return Verdict{}, errors.New("criteria download bytes cannot be negative")
	}
	if err := os.MkdirAll(out.IOSDir, 0o700); err != nil {
		return Verdict{}, fmt.Errorf("create ios summary dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(out.ServerSummary), 0o700); err != nil {
		return Verdict{}, fmt.Errorf("create server summary dir: %w", err)
	}

	verdict := Verdict{Green: true}
	appLog, err := findIOSLog(raw.IOSDir, "app.log")
	if err != nil {
		return Verdict{}, err
	}
	tunnelLog, err := findIOSLog(raw.IOSDir, "tunnel.log")
	if err != nil {
		return Verdict{}, err
	}

	appLines, appStats, err := summarizeApp(appLog)
	if err != nil {
		return Verdict{}, err
	}
	tunnelLines, tunnelStats, err := summarizeTunnel(tunnelLog)
	if err != nil {
		return Verdict{}, err
	}
	serverLines, serverStats, err := summarizeServer(raw.ServerLog)
	if err != nil {
		return Verdict{}, err
	}

	if err := writeSummary(filepath.Join(out.IOSDir, "app-probes-summary.log"), appLines); err != nil {
		return Verdict{}, err
	}
	if err := writeSummary(filepath.Join(out.IOSDir, "tunnel-readiness-summary.log"), tunnelLines); err != nil {
		return Verdict{}, err
	}
	if err := writeSummary(out.ServerSummary, serverLines); err != nil {
		return Verdict{}, err
	}

	verdict.RoundOK = appStats.roundOK
	verdict.DownloadOK = appStats.downloadOK
	verdict.HTTPError = appStats.httpErrors
	verdict.Reconnects = serverStats.reconnects + tunnelStats.reconnects

	if verdict.RoundOK != criteria.Rounds {
		verdict.Reasons = append(verdict.Reasons,
			fmt.Sprintf("round ok count %d != expected rounds %d", verdict.RoundOK, criteria.Rounds))
	}
	if criteria.DownloadBytes > 0 && verdict.DownloadOK != criteria.Rounds {
		verdict.Reasons = append(verdict.Reasons,
			fmt.Sprintf("download ok count %d != expected rounds %d", verdict.DownloadOK, criteria.Rounds))
	}
	if verdict.HTTPError > 0 {
		verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("http probe errors: %d", verdict.HTTPError))
	}
	if verdict.Reconnects > 0 {
		verdict.Reasons = append(verdict.Reasons,
			fmt.Sprintf("server reconnect/teardown events: %d", verdict.Reconnects))
	}
	verdict.Green = len(verdict.Reasons) == 0
	return verdict, nil
}

func findIOSLog(dir, name string) (string, error) {
	for _, candidate := range []string{
		filepath.Join(dir, name),
		filepath.Join(dir, "olc", name),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("iOS log %s not found under %s or %s", name, dir, filepath.Join(dir, "olc"))
}

type appStats struct {
	roundOK    int
	downloadOK int
	httpErrors int
}

type transportStats struct {
	reconnects int
}

var (
	appSummaryPattern = regexp.MustCompile(`profile override|connect start|connect ok|http probe`)
	downloadOKPattern = regexp.MustCompile(`http probe ok label=download .* bytes=([0-9]+)`)
	roundDonePattern  = regexp.MustCompile(`http probe round=[0-9]+ done ok=[0-9]+ fail=([0-9]+)`)
	httpErrorPattern  = regexp.MustCompile(`http probe error`)

	tunnelSummaryPattern = regexp.MustCompile(`=== startTunnel|cnc start|cnc session ready|network settings applied|SOCKS ready|WARN: cnc reported|cnc not ready|tun2socks starting|tun2socks stats|cnc ENDED|stopTunnel|publisher PC closed|ICE connection state: closed|readVP8Track closed`)
	tunnelBadPattern     = regexp.MustCompile(`cnc not ready|cnc ENDED err|publisher PC closed|ICE connection state: closed|readVP8Track closed`)

	serverSummaryPattern = regexp.MustCompile(`peer connected|traffic: session=.*(speed\.cloudflare\.com|example\.com|api\.ipify\.org)|server reconnect|tearing down|publisher PC closed|readVP8Track closed`)
	serverBadPattern     = regexp.MustCompile(`server reconnect|tearing down|publisher PC closed|readVP8Track closed`)

	uuidPattern      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	roomURLPattern   = regexp.MustCompile(`https://telemost\.yandex\.ru/j/[A-Za-z0-9._-]+`)
	longDigitPattern = regexp.MustCompile(`\b[0-9]{12,}\b`)
)

func summarizeApp(path string) ([]string, appStats, error) {
	lines, err := selectLines(path, appSummaryPattern)
	if err != nil {
		return nil, appStats{}, fmt.Errorf("summarize app log: %w", err)
	}
	stats := appStats{}
	for _, line := range lines {
		if httpErrorPattern.MatchString(line) {
			stats.httpErrors++
		}
		if match := roundDonePattern.FindStringSubmatch(line); match != nil && match[1] == "0" {
			stats.roundOK++
		}
		if match := downloadOKPattern.FindStringSubmatch(line); match != nil {
			bytes, _ := strconv.ParseInt(match[1], 10, 64)
			if bytes > 0 {
				stats.downloadOK++
			}
		}
	}
	return lines, stats, nil
}

func summarizeTunnel(path string) ([]string, transportStats, error) {
	lines, err := selectLines(path, tunnelSummaryPattern)
	if err != nil {
		return nil, transportStats{}, fmt.Errorf("summarize tunnel log: %w", err)
	}
	stats := transportStats{}
	for _, line := range lines {
		if tunnelBadPattern.MatchString(line) {
			stats.reconnects++
		}
	}
	return lines, stats, nil
}

func summarizeServer(path string) ([]string, transportStats, error) {
	lines, err := selectLines(path, serverSummaryPattern)
	if err != nil {
		return nil, transportStats{}, fmt.Errorf("summarize server log: %w", err)
	}
	stats := transportStats{}
	for _, line := range lines {
		if serverBadPattern.MatchString(line) {
			stats.reconnects++
		}
	}
	return lines, stats, nil
}

func selectLines(path string, pattern *regexp.Regexp) ([]string, error) {
	file, err := os.Open(path) // #nosec G304 -- explicit local harness path
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if pattern.MatchString(line) {
			lines = append(lines, sanitize(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func sanitize(line string) string {
	line = roomURLPattern.ReplaceAllString(line, "https://telemost.yandex.ru/j/<room>")
	line = uuidPattern.ReplaceAllString(line, "<uuid>")
	line = longDigitPattern.ReplaceAllString(line, "<id>")
	return line
}

func writeSummary(path string, lines []string) error {
	data := strings.Join(lines, "\n")
	if data != "" {
		data += "\n"
	}
	return os.WriteFile(path, []byte(data), 0o600)
}
