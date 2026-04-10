package main

import (
	"os/exec"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

type Program struct {
	App          fyne.App
	ParentWindow fyne.Window
	RunString    string
	Config       *Config
	Cmd          *exec.Cmd
}

func main() {
	log("Starting application...")
	program := NewProgram()
	program.Run()
}

func NewProgram() *Program {
	log("Initializing program...")
	cfg := loadConfig()
	p := &Program{
		App:    app.New(),
		Config: cfg,
	}
	p.buildRunString(cfg.ConferenceID, cfg.EncryptionKey, cfg.SocksPort, cfg.DNS)
	return p
}

func (p *Program) Run() {
	log("Creating main window...")
	w := p.App.NewWindow("OlcRTC")
	w.CenterOnScreen()
	w.Resize(fyne.NewSize(1280, 700))
	p.ParentWindow = w

	settingsBtn := widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), func() {
		log("Settings button clicked")
		p.settingsWindow()
	})

	runCheck := widget.NewCheck("Run", func(b bool) {
		if b {
			log("Run enabled")
			p.olcrtcRun()
		} else {
			log("Run disabled")
			p.olcrtcStop()
		}
	})

	w.SetContent(container.NewBorder(
		settingsBtn,
		runCheck, nil, nil,
	))
	log("Window created and running...")
	w.ShowAndRun()
}
