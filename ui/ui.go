package main

import (
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func (p *Program) settingsWindow() {
	log("Opening settings dialog...")

	providerSelect := widget.NewSelect([]string{"telemost", "jazz", "wb_stream"}, nil)
	if p.Config.Provider != "" {
		providerSelect.SetSelected(p.Config.Provider)
	} else {
		providerSelect.SetSelected("telemost")
	}

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

	roomPassword := widget.NewPasswordEntry()
	if p.Config.RoomPassword != "" {
		roomPassword.SetText(p.Config.RoomPassword)
	}

	roomIdLabel := widget.NewLabel("Room ID (telemost: numbers only, others: any)")
	roomPasswordLabel := widget.NewLabel("Room Password (jazz only)")
	roomPasswordContainer := container.NewVBox(roomPasswordLabel, roomPassword)

	providerSelect.OnChanged = func(value string) {
		log("Provider selected: %s", value)
		if value == "jazz" {
			roomIdLabel.SetText("Room ID (jazz: any)")
			roomPasswordContainer.Show()
		} else if value == "wb_stream" {
			roomIdLabel.SetText("Room ID (wb_stream: any)")
			roomPasswordContainer.Hide()
		} else {
			roomIdLabel.SetText("Room ID (telemost: numbers only)")
			roomPasswordContainer.Hide()
		}
	}

	if providerSelect.Selected != "jazz" {
		roomPasswordContainer.Hide()
	}

	applyBtn := widget.NewButtonWithIcon("Apply", theme.CheckButtonCheckedIcon(), func() {
		log("Applying settings...")
		p.buildRunString(conferenceId.Text, roomPassword.Text, encrpKey.Text, socksPort.Text, dns.Text, providerSelect.Selected)
		p.saveConfig(dns.Text, encrpKey.Text, socksPort.Text, conferenceId.Text, roomPassword.Text, providerSelect.Selected)
	})

	content := container.NewVBox(
		widget.NewLabel("Provider"),
		providerSelect,
		widget.NewLabel("Custom DNS Server"),
		dns,
		widget.NewLabel("Encryption Key"),
		encrpKey,
		widget.NewLabel("Socks Port"),
		socksPort,
		roomIdLabel,
		conferenceId,
		roomPasswordContainer,
		applyBtn,
	)
	dialog.ShowCustom("Settings", "Close", content, p.ParentWindow)
}

func (p *Program) buildRunString(conferenceId, roomPassword, encryptionKey, socksPort, dns, provider string) {
	log("Building run string...")
	log("  Provider: %s", provider)
	log("  Conference ID: %s", conferenceId)
	log("  Room Password: %s", roomPassword)
	log("  Encryption Key: %s", encryptionKey)
	log("  Socks Port: %s", socksPort)
	log("  DNS Server: %s", dns)

	finalRoomId := conferenceId
	if provider == "jazz" && roomPassword != "" {
		finalRoomId = conferenceId + ":" + roomPassword
	}

	switch p.Config.Os {
	case "windows":
		p.RunString = fmt.Sprintf("olcrtc.exe -mode cnc -provider %s -id \"%s\" -key \"%s\" -socks-port %s -dns %s", provider, finalRoomId, encryptionKey, socksPort, dns)
	case "linux", "darwin":
		p.RunString = fmt.Sprintf("./olcrtc -mode cnc -provider %s -id \"%s\" -key \"%s\" -socks-port %s -dns %s", provider, finalRoomId, encryptionKey, socksPort, dns)
	default:
		p.RunString = fmt.Sprintf("olcrtc -mode cnc -provider %s -id \"%s\" -key \"%s\" -socks-port %s -dns %s", provider, finalRoomId, encryptionKey, socksPort, dns)
	}
	log("Generated command: %s", p.RunString)
}

func (p *Program) showError(err error) {
	dialog.ShowError(err, p.ParentWindow)
}

func (p *Program) MarkUncheck() {
	fyne.Do(func() { p.RunCheck.SetChecked(false) })
}
