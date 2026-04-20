package wbstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

const apiBase = "https://stream.wb.ru"

type GuestRegisterRequest struct {
	DisplayName string `json:"displayName"`
	Device      Device `json:"device"`
}

type Device struct {
	DeviceName string `json:"deviceName"`
	DeviceType string `json:"deviceType"`
}

type GuestRegisterResponse struct {
	AccessToken string `json:"accessToken"`
}

type CreateRoomRequest struct {
	RoomType    string `json:"roomType"`
	RoomPrivacy string `json:"roomPrivacy"`
}

type CreateRoomResponse struct {
	RoomID string `json:"roomId"`
}

type TokenResponse struct {
	RoomToken string `json:"roomToken"`
}

func RegisterGuest(ctx context.Context, displayName string) (string, error) {
	u := apiBase + "/auth/api/v1/auth/user/guest-register"
	reqBody := GuestRegisterRequest{
		DisplayName: displayName,
		Device: Device{
			DeviceName: "Linux",
			DeviceType: "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP",
		},
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux x86_64)")

	client := protect.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("guest register failed: %d %s", resp.StatusCode, b)
	}

	var res GuestRegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.AccessToken, nil
}

func CreateRoom(ctx context.Context, accessToken string) (string, error) {
	u := apiBase + "/api-room/api/v2/room"
	reqBody := CreateRoomRequest{
		RoomType:    "ROOM_TYPE_ALL_ON_SCREEN",
		RoomPrivacy: "ROOM_PRIVACY_FREE",
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux x86_64)")

	client := protect.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create room failed: %d %s", resp.StatusCode, b)
	}

	var res CreateRoomResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.RoomID, nil
}

func JoinRoom(ctx context.Context, accessToken, roomID string) error {
	u := fmt.Sprintf("%s/api-room/api/v1/room/%s/join", apiBase, roomID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux x86_64)")

	client := protect.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("join room failed: %d %s", resp.StatusCode, b)
	}
	return nil
}

func GetToken(ctx context.Context, accessToken, roomID, displayName string) (string, error) {
	u := fmt.Sprintf("%s/api-room-manager/api/v1/room/%s/token", apiBase, roomID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}

	q := req.URL.Query()
	q.Add("deviceType", "PARTICIPANT_DEVICE_TYPE_WEB_DESKTOP")
	q.Add("displayName", displayName)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux x86_64)")

	client := protect.NewHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get token failed: %d %s", resp.StatusCode, b)
	}

	var res TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.RoomToken, nil
}
