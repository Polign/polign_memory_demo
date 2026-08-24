// Script mode replays a file of user lines through the real agent loop, so a
// demo run can be recorded without anyone typing. The lines are printed after
// the prompt with a typing delay, so the recording reads like a live session.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	typeDelay = 35 * time.Millisecond
	turnPause = 900 * time.Millisecond
)

// loadScript reads a demo script: one user line per line; blank lines and
// lines starting with # are skipped.
func loadScript(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s: no script lines", path)
	}
	return lines, nil
}

// scriptSource yields the script lines one per turn, printing each after the
// prompt as if typed. perRune and pause are parameters so tests pass zero.
func scriptSource(lines []string, perRune, pause time.Duration) func() (string, bool) {
	i := 0
	return func() (string, bool) {
		if i >= len(lines) {
			return "", false
		}
		if i > 0 {
			time.Sleep(pause)
		}
		line := lines[i]
		i++
		fmt.Print("\nyou> ")
		typeOut(line, perRune)
		fmt.Println()
		return line, true
	}
}

// typeOut prints s one rune at a time with perRune delay between runes.
func typeOut(s string, perRune time.Duration) {
	for _, r := range s {
		fmt.Print(string(r))
		time.Sleep(perRune)
	}
}
