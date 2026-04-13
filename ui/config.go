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
	DNS           string `json:"dns"` // todo
	EncryptionKey string `json:"encryption_key"`
	SocksPort     string `json:"socks_port"`
	ConferenceID  string `json:"conference_id"`
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
	// default values
	cfg := &Config{
		DNS:           "77.88.8.8",
		EncryptionKey: "",
		SocksPort:     "1080",
		ConferenceID:  "",
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
	if !isValidConferenceID(cfg.ConferenceID) {
		log("WARNING: Invalid conference ID in config (must be numbers only)")
		cfg.ConferenceID = ""
	}
	if !isValidPort(cfg.SocksPort) {
		log("WARNING: Invalid port in config, using default: 1080")
		cfg.SocksPort = "1080"
	}
	log("Config loaded successfully")
	return cfg
}

func (p *Program) saveConfig(dns, encryptionKey, socksPort, conferenceID string) {
	log("Saving configuration...")

	conferenceID = strings.ReplaceAll(conferenceID, " ", "")

	if !isValidPort(socksPort) {
		log("ERROR: Invalid port: %s", socksPort)
		p.showError(fmt.Errorf("invalid port: must be between 1 and 65535"))
		return
	}

	if !isValidConferenceID(conferenceID) {
		log("ERROR: Invalid conference ID: %s", conferenceID)
		p.showError(fmt.Errorf("invalid conference ID: must contain only numbers"))
		return
	}

	p.Config = &Config{
		DNS:           dns,
		EncryptionKey: encryptionKey,
		SocksPort:     socksPort,
		ConferenceID:  conferenceID,
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
