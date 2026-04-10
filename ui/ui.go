package main

import (
	"fmt"

	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func (p *Program) settingsWindow() {
	log("Opening settings dialog...")

	dns := widget.NewEntry()
	dns.SetPlaceHolder("1.1.1.1")
	if p.Config.DNS != "" {
		dns.SetText(p.Config.DNS)
	}

	encrpKey := widget.NewEntry()
	if p.Config.EncryptionKey != "" {
		encrpKey.SetText(p.Config.EncryptionKey)
	}

	socksPort := widget.NewEntry()
	socksPort.SetPlaceHolder("1080")
	if p.Config.SocksPort != "" {
		socksPort.SetText(p.Config.SocksPort)
	}

	conferenceId := widget.NewEntry()
	if p.Config.ConferenceID != "" {
		conferenceId.SetText(p.Config.ConferenceID)
	}

	applyBtn := widget.NewButtonWithIcon("Apply", theme.CheckButtonCheckedIcon(), func() {
		log("Applying settings...")
		p.buildRunString(conferenceId.Text, encrpKey.Text, socksPort.Text, dns.Text)
		p.saveConfig(dns.Text, encrpKey.Text, socksPort.Text, conferenceId.Text)
	})

	content := container.NewVBox(
		widget.NewLabel("Custom DNS Server"),
		dns,
		widget.NewLabel("Encryption Key"),
		encrpKey,
		widget.NewLabel("Socks Port"),
		socksPort,
		widget.NewLabel("Conference ID"),
		conferenceId,
		applyBtn,
	)
	dialog.ShowCustom("Settings", "Close", content, p.ParentWindow)
}

func (p *Program) buildRunString(conferenceId, encryptionKey, socksPort, dns string) {
	log("Building run string...")
	log("  Conference ID: %s", conferenceId)
	log("  Encryption Key: %s", encryptionKey)
	log("  Socks Port: %s", socksPort)
	log("  DNS Server: %s", dns)

	p.RunString = fmt.Sprintf("./olcrtc -mode cnc -id \"%s\" -key \"%s\" -socks-port %s", conferenceId, encryptionKey, socksPort)
	log("Generated command: %s", p.RunString)
}

func (p *Program) showError(err error) {
	dialog.ShowError(err, p.ParentWindow)
}
