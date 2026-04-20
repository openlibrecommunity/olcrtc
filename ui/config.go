package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	Os            string
	DNS           string `json:"dns"`
	EncryptionKey string `json:"encryption_key"`
	SocksPort     string `json:"socks_port"`
	ConferenceID  string `json:"conference_id"`
	RoomPassword  string `json:"room_password"`
	Provider      string `json:"provider"`
}

func isValidPort(portStr string) bool {
	portStr = strings.TrimSpace(portStr)
	if portStr == "" {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return false
	}
	return port > 0 && port <= 65535
}

func isValidConferenceID(conferenceID string) bool {
	conferenceID = strings.TrimSpace(conferenceID)
	if conferenceID == "" {
		return false
	}
	matched, err := regexp.MatchString(`^\d+$`, conferenceID)
	if err != nil {
		return false
	}
	return matched
}

func (p *Program) getConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		log("WARNING: Could not get system config directory: %v", err)
		return "config.json"
	}
	configDir := filepath.Join(dir, "olcrtc")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		log("WARNING: Could not create config directory: %v", err)
	}
	return filepath.Join(configDir, "config.json")
}

func (p *Program) loadConfig() *Config {
	configPath := p.getConfigPath()
	log("Loading config from: %s", configPath)
	cfg := &Config{
		DNS:           "1.1.1.1",
		EncryptionKey: "",
		SocksPort:     "1080",
		ConferenceID:  "",
		RoomPassword:  "",
		Provider:      "telemost",
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log("Config file not found. Using default configuration.")
		} else {
			log("WARNING: Could not read config file: %v", err)
		}
		return cfg
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		log("WARNING: Could not parse config file: %v", err)
		return cfg
	}
	cfg.ConferenceID = strings.ReplaceAll(cfg.ConferenceID, " ", "")
	
	// Validation check for telemost specifically if it was stored
	if cfg.Provider == "telemost" && !isValidConferenceID(cfg.ConferenceID) {
		log("WARNING: Invalid conference ID in config (must be numbers only for telemost)")
		cfg.ConferenceID = ""
	}
	
	if !isValidPort(cfg.SocksPort) {
		log("WARNING: Invalid port in config, using default: 1080")
		cfg.SocksPort = "1080"
	}
	log("Config loaded successfully")
	return cfg
}

func (p *Program) saveConfig(dns, encryptionKey, socksPort, conferenceID, roomPassword, provider string) {
	log("Saving configuration...")

	conferenceID = strings.ReplaceAll(conferenceID, " ", "")
	roomPassword = strings.ReplaceAll(roomPassword, " ", "")

	if !isValidPort(socksPort) {
		log("ERROR: Invalid port: %s", socksPort)
		p.showError(fmt.Errorf("invalid port: must be between 1 and 65535"))
		return
	}

	if provider == "telemost" && !isValidConferenceID(conferenceID) {
		log("ERROR: Invalid conference ID for telemost: %s", conferenceID)
		p.showError(fmt.Errorf("invalid conference ID: must contain only numbers for telemost"))
		return
	}

	if (provider == "jazz" || provider == "wb_stream") && conferenceID == "" {
		log("ERROR: Room ID required for %s provider", provider)
		p.showError(fmt.Errorf("room ID required for %s provider", provider))
		return
	}

	if provider != "telemost" && provider != "jazz" && provider != "wb_stream" {
		log("ERROR: Invalid provider: %s", provider)
		p.showError(fmt.Errorf("invalid provider: must be telemost, jazz or wb_stream"))
		return
	}

	currentOs := p.Config.Os

	p.Config = &Config{
		Os:            currentOs,
		DNS:           dns,
		EncryptionKey: encryptionKey,
		SocksPort:     socksPort,
		ConferenceID:  conferenceID,
		RoomPassword:  roomPassword,
		Provider:      provider,
	}

	configPath := p.getConfigPath()
	data, err := json.MarshalIndent(p.Config, "", "  ")
	if err != nil {
		log("ERROR: Could not marshal config: %v", err)
		p.showError(err)
		return
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		log("ERROR: Could not write config file: %v", err)
		p.showError(err)
		return
	}

	log("Configuration saved to: %s", configPath)
}
