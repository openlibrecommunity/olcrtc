package main

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
)

type Config struct {
	DNS           string `json:"dns"` // на потом
	EncryptionKey string `json:"encryption_key"`
	SocksPort     string `json:"socks_port"`
	ConferenceID  string `json:"conference_id"`
}

func getConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log("WARNING: Could not get home directory: %v", err)
		return "./olcrtc_config.json"
	}
	configDir := filepath.Join(home, ".olcrtc")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		log("WARNING: Could not create config directory: %v", err)
	}
	return filepath.Join(configDir, "config.json")
}

func loadConfig() *Config {
	configPath := getConfigPath()
	log("Loading config from: %s", configPath)

	cfg := &Config{
		DNS:           "1.1.1.1",
		EncryptionKey: "",
		SocksPort:     "1080",
		ConferenceID:  "",
	}

	data, err := ioutil.ReadFile(configPath)
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

	log("Config loaded successfully")
	return cfg
}

func (p *Program) saveConfig(dns, encryptionKey, socksPort, conferenceID string) {
	log("Saving configuration...")

	p.Config = &Config{
		DNS:           dns,
		EncryptionKey: encryptionKey,
		SocksPort:     socksPort,
		ConferenceID:  conferenceID,
	}

	configPath := getConfigPath()
	data, err := json.MarshalIndent(p.Config, "", "  ")
	if err != nil {
		log("ERROR: Could not marshal config: %v", err)
		p.showError(err)
		return
	}

	if err := ioutil.WriteFile(configPath, data, 0644); err != nil {
		log("ERROR: Could not write config file: %v", err)
		p.showError(err)
		return
	}

	log("Configuration saved to: %s", configPath)
}
