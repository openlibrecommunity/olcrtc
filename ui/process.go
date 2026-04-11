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
		p.MarkUncheck()
		return
	}

	var cmd *exec.Cmd
	if p.Config.Os == "windows" {
		cmd = exec.Command("cmd.exe", "/C", p.RunString)
	} else {
		cmd = exec.Command("sh", "-c", p.RunString)
	}

	p.CmdMu.Lock()
	p.Cmd = cmd
	err := p.Cmd.Start()
	pid := 0
	if err == nil && p.Cmd.Process != nil {
		pid = p.Cmd.Process.Pid
	}
	p.CmdMu.Unlock()

	if err != nil {
		log("ERROR: Failed to start olcrtc: %v", err)
		p.showError(err)
		p.CmdMu.Lock()
		p.Cmd = nil
		p.CmdMu.Unlock()
		p.MarkUncheck()
	} else {
		log("olcrtc process started (PID: %d)", pid)
		go func() {
			p.CmdMu.Lock()
			cmd := p.Cmd
			p.CmdMu.Unlock()

			if cmd != nil {
				err = cmd.Wait()
				if err != nil {
					log("olcrtc process exited with error: %v", err)
					p.MarkUncheck()
				} else {
					log("olcrtc process exited successfully")
				}
			}

			p.CmdMu.Lock()
			p.Cmd = nil
			p.CmdMu.Unlock()
		}()
	}
}

func (p *Program) olcrtcStop() {
	log("%s - Stopping olcrtc process...", time.Now().Format("2006-01-02 15:04:05"))

	p.CmdMu.Lock()
	cmd := p.Cmd
	p.CmdMu.Unlock()

	if cmd == nil || cmd.Process == nil {
		log("WARNING: No active olcrtc process to stop")
		return
	}

	var err error
	if p.Config.Os == "windows" {
		err = cmd.Process.Kill()
	} else {
		err = cmd.Process.Signal(os.Interrupt)
	}
	if err != nil {
		log("ERROR: Failed to signal olcrtc: %v", err)
		p.showError(err)
		return
	}

	log("olcrtc process termination signal sent (PID: %d)", cmd.Process.Pid)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-time.After(5 * time.Second):
		log("WARNING: Process did not exit gracefully, forcing kill...")
		if err := cmd.Process.Kill(); err != nil {
			log("ERROR: Failed to kill olcrtc: %v", err)
		} else {
			log("olcrtc process forcefully killed (PID: %d)", cmd.Process.Pid)
		}
	case err := <-done:
		if err != nil {
			log("olcrtc process exited with error: %v", err)
		} else {
			log("olcrtc process exited gracefully")
		}
	}
}
