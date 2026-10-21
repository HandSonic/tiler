package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// Pause stops scheduling new tiles. In-flight requests are allowed to finish.
func (task *Task) Pause() {
	task.stateMu.Lock()
	defer task.stateMu.Unlock()
	if !task.paused {
		task.resumeSignal = make(chan struct{})
		task.paused = true
	}
}

// Resume continues a paused task.
func (task *Task) Resume() {
	task.stateMu.Lock()
	defer task.stateMu.Unlock()
	if task.paused {
		close(task.resumeSignal)
		task.paused = false
	}
}

// Cancel stops scheduling and cancels in-flight HTTP and S3 requests.
func (task *Task) Cancel() {
	task.cancel()
}

func (task *Task) waitUntilRunning(ctx context.Context) error {
	for {
		task.stateMu.Lock()
		paused, signal := task.paused, task.resumeSignal
		task.stateMu.Unlock()
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-signal:
		}
	}
}

// Console commands are line based so they work in Windows 7 cmd.exe as well.
func (task *Task) readControls(input io.Reader, output io.Writer) {
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "p", "pause":
			task.Pause()
			fmt.Fprintln(output, "Paused. Enter r to resume, q to stop.")
		case "r", "resume":
			task.Resume()
			fmt.Fprintln(output, "Resumed.")
		case "q", "quit":
			task.Cancel()
			return
		}
	}
}
