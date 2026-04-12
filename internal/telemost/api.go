package telemost //nolint:revive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

const apiBase = "https://cloud-api.yandex.ru/telemost_front/v2/telemost"

var ErrAPI = errors.New("api error") //nolint:revive

type ConnectionInfo struct { //nolint:revive
	RoomID       string `json:"room_id"`             //nolint:tagliatelle
	PeerID       string `json:"peer_id"`             //nolint:tagliatelle
	Credentials  string `json:"credentials"`         //nolint:tagliatelle
	ClientConfig struct {
		MediaServerURL string `json:"media_server_url"` //nolint:tagliatelle
	} `json:"client_configuration"` //nolint:tagliatelle
}

func GetConnectionInfo(ctx context.Context, roomURL, displayName string) (*ConnectionInfo, error) { //nolint:revive
	u := fmt.Sprintf("%s/conferences/%s/connection", apiBase, url.QueryEscape(roomURL))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	q := req.URL.Query()
	q.Add("next_gen_media_platform_allowed", "true")
	q.Add("display_name", displayName)
	q.Add("waiting_room_supported", "true")
	req.URL.RawQuery = q.Encode()

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-Instance-Id", uuid.New().String())
	req.Header.Set("X-Telemost-Client-Version", "187.1.0")
	req.Header.Set("Idempotency-Key", uuid.New().String())
	req.Header.Set("Origin", "https://telemost.yandex.ru")
	req.Header.Set("Referer", "https://telemost.yandex.ru/")

	client := protect.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w %d: %s", ErrAPI, resp.StatusCode, body)
	}

	var info ConnectionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &info, nil
}
