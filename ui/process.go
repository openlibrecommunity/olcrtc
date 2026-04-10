package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func (p *Program) olcrtcRun() {
	log("%s - Starting olcrtc process...", time.Now().Format("2006-01-02 15:04:05"))
	if p.RunString == "" {
		log("ERROR: Run string is empty. Please configure settings first.")
		p.showError(fmt.Errorf("run string is empty - please configure settings"))
		return
	}

	p.Cmd = exec.Command("sh", "-c", p.RunString)
	err := p.Cmd.Start()
	if err != nil {
		log("ERROR: Failed to start olcrtc: %v", err)
		p.showError(err)
		p.Cmd = nil
	} else {
		log("olcrtc process started (PID: %d)", p.Cmd.Process.Pid)
		go func() {
			err := p.Cmd.Wait()
			if err != nil {
				log("olcrtc process exited with error: %v", err)
			} else {
				log("olcrtc process exited successfully")
			}
			p.Cmd = nil
		}()
	}
}

func (p *Program) olcrtcStop() {
	log("%s - Stopping olcrtc process...", time.Now().Format("2006-01-02 15:04:05"))
	if p.Cmd == nil || p.Cmd.Process == nil {
		log("WARNING: No active olcrtc process to stop")
		return
	}

	err := p.Cmd.Process.Signal(os.Interrupt)
	if err != nil {
		log("ERROR: Failed to signal olcrtc: %v", err)
		p.showError(err)
	} else {
		log("olcrtc process termination signal sent (PID: %d)", p.Cmd.Process.Pid)
	}
}
