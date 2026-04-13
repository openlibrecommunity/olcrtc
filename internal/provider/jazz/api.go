package jazz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

const apiBase = "https://bk.salutejazz.ru"

type RoomInfo struct {
	RoomID       string `json:"roomId"`
	Password     string `json:"password"`
	ConnectorURL string `json:"connectorUrl"`
}

var (
	errCreateRoomFailed = errors.New("create room failed")
	errPreconnectFailed = errors.New("preconnect failed")
)

func createRoom(ctx context.Context) (*RoomInfo, error) {
	clientID := uuid.New().String()
	headers := map[string]string{
		"X-Jazz-ClientId":   clientID,
		"X-Jazz-AuthType":   "ANONYMOUS",
		"X-Client-AuthType": "ANONYMOUS",
		"Content-Type":      "application/json",
	}

	createPayload := map[string]any{
		"title":                             "olcrtc",
		"guestEnabled":                      true,
		"lobbyEnabled":                      false,
		"serverVideoRecordAutoStartEnabled": false,
		"sipEnabled":                        false,
		"moderatorEmails":                   []string{},
		"summarizationEnabled":              false,
		"room3dEnabled":                     false,
		"room3dScene":                       "XRLobby",
	}

	body, err := json.Marshal(createPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal create payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/room/create-meeting",
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := protect.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do create request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errCreateRoomFailed, resp.StatusCode)
	}

	var createResp struct {
		RoomID   string `json:"roomId"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("decode create response: %w", err)
	}

	preconnectPayload := map[string]any{
		"password": createResp.Password,
		"jazzNextMigration": map[string]any{
			"b2bBaseRoomSupport":               true,
			"demoRoomBaseSupport":              true,
			"demoRoomVersionSupport":           2,
			"mediaWithoutAutoSubscribeSupport": true,
			"webinarSpeakerSupport":            true,
			"webinarViewerSupport":             true,
			"sdkRoomSupport":                   true,
			"sberclassRoomSupport":             true,
		},
	}

	preBody, err := json.Marshal(preconnectPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal preconnect payload: %w", err)
	}

	preReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/room/%s/preconnect", apiBase, createResp.RoomID),
		bytes.NewReader(preBody),
	)
	if err != nil {
		return nil, fmt.Errorf("create preconnect request: %w", err)
	}

	for k, v := range headers {
		preReq.Header.Set(k, v)
	}

	preResp, err := client.Do(preReq)
	if err != nil {
		return nil, fmt.Errorf("do preconnect request: %w", err)
	}
	defer func() { _ = preResp.Body.Close() }()

	if preResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errPreconnectFailed, preResp.StatusCode)
	}

	var preconnectResp struct {
		ConnectorURL string `json:"connectorUrl"`
	}
	if err := json.NewDecoder(preResp.Body).Decode(&preconnectResp); err != nil {
		return nil, fmt.Errorf("decode preconnect response: %w", err)
	}

	return &RoomInfo{
		RoomID:       createResp.RoomID,
		Password:     createResp.Password,
		ConnectorURL: preconnectResp.ConnectorURL,
	}, nil
}

func joinRoom(ctx context.Context, roomID, password string) (*RoomInfo, error) {
	clientID := uuid.New().String()
	headers := map[string]string{
		"X-Jazz-ClientId":   clientID,
		"X-Jazz-AuthType":   "ANONYMOUS",
		"X-Client-AuthType": "ANONYMOUS",
		"Content-Type":      "application/json",
	}

	preconnectPayload := map[string]any{
		"password": password,
		"jazzNextMigration": map[string]any{
			"b2bBaseRoomSupport":               true,
			"demoRoomBaseSupport":              true,
			"demoRoomVersionSupport":           2,
			"mediaWithoutAutoSubscribeSupport": true,
			"webinarSpeakerSupport":            true,
			"webinarViewerSupport":             true,
			"sdkRoomSupport":                   true,
			"sberclassRoomSupport":             true,
		},
	}

	preBody, err := json.Marshal(preconnectPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal preconnect payload: %w", err)
	}

	preReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/room/%s/preconnect", apiBase, roomID),
		bytes.NewReader(preBody),
	)
	if err != nil {
		return nil, fmt.Errorf("create preconnect request: %w", err)
	}

	for k, v := range headers {
		preReq.Header.Set(k, v)
	}

	client := protect.NewHTTPClient()
	preResp, err := client.Do(preReq)
	if err != nil {
		return nil, fmt.Errorf("do preconnect request: %w", err)
	}
	defer func() { _ = preResp.Body.Close() }()

	if preResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errPreconnectFailed, preResp.StatusCode)
	}

	var preconnectResp struct {
		ConnectorURL string `json:"connectorUrl"`
	}
	if err := json.NewDecoder(preResp.Body).Decode(&preconnectResp); err != nil {
		return nil, fmt.Errorf("decode preconnect response: %w", err)
	}

	return &RoomInfo{
		RoomID:       roomID,
		Password:     password,
		ConnectorURL: preconnectResp.ConnectorURL,
	}, nil
}
