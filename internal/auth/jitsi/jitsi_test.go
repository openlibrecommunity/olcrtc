package jitsi

import (
	"context"
	"errors"
	"testing"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
)

const (
	testRoom = "myroom"
	testHost = "meet.example"
)

func TestParseRoomURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		host    string
		room    string
		wantErr bool
	}{
		{name: "https url", raw: "https://meet.systemli.org/" + testRoom, host: "meet.systemli.org", room: testRoom},
		{name: "http url", raw: "http://" + testHost + "/" + testRoom, host: testHost, room: testRoom},
		{name: "scheme-less", raw: "meet.example.com/" + testRoom, host: "meet.example.com", room: testRoom},
		{name: "trailing slash", raw: "https://" + testHost + "/" + testRoom + "/", host: testHost, room: testRoom},
		{name: "double slash leader", raw: "//" + testHost + "/" + testRoom, host: testHost, room: testRoom},
		{name: "uppercase room", raw: "https://" + testHost + "/MyRoom", host: testHost, room: "MyRoom"},
		{name: "empty", raw: "", wantErr: true},
		{name: "host only", raw: "meet.example.com", wantErr: true},
		{name: "no room", raw: "https://" + testHost + "/", wantErr: true},
		{name: "scheme only", raw: "https://", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, room, err := parseRoomURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRoomURL(%q) = (%q, %q), want error", tc.raw, host, room)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRoomURL(%q) error = %v, want nil", tc.raw, err)
			}
			if host != tc.host || room != tc.room {
				t.Fatalf("parseRoomURL(%q) = (%q, %q), want (%q, %q)",
					tc.raw, host, room, tc.host, tc.room)
			}
		})
	}
}

func TestProviderIssue(t *testing.T) {
	creds, err := Provider{}.Issue(context.Background(), auth.Config{
		RoomURL: "https://meet.systemli.org/olcrtc",
		Name:    "olcrtc-test",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if creds.URL != "meet.systemli.org" {
		t.Fatalf("URL = %q, want %q", creds.URL, "meet.systemli.org")
	}
	if got := creds.Extra[CredentialKeyRoom]; got != "olcrtc" {
		t.Fatalf("room = %q, want %q", got, "olcrtc")
	}
	if creds.Token != "" {
		t.Fatalf("Token = %q, want empty", creds.Token)
	}
}

func TestProviderIssueRequiresRoom(t *testing.T) {
	_, err := Provider{}.Issue(context.Background(), auth.Config{RoomURL: ""})
	if !errors.Is(err, auth.ErrRoomIDRequired) {
		t.Fatalf("Issue() err = %v, want ErrRoomIDRequired", err)
	}
}

func TestProviderEngine(t *testing.T) {
	if got := (Provider{}).Engine(); got != "jitsi" {
		t.Fatalf("Engine() = %q, want %q", got, "jitsi")
	}
	if got := (Provider{}).DefaultServiceURL(); got != defaultServiceURL {
		t.Fatalf("DefaultServiceURL() = %q, want %q", got, defaultServiceURL)
	}
}
