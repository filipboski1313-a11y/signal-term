package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func ReadClipboard() string {
	if wlPaste, err := exec.LookPath("wl-paste"); err == nil {
		typesOut, _ := exec.Command(wlPaste, "--list-types").Output()
		types := strings.Split(string(typesOut), "\n")
		for _, pair := range [][2]string{
			{"image/png", ".png"},
			{"image/jpeg", ".jpg"},
			{"image/webp", ".webp"},
		} {
			for _, t := range types {
				if strings.TrimSpace(t) == pair[0] {
					imgData, err := exec.Command(wlPaste, "--type", pair[0]).Output()
					if err == nil && len(imgData) > 0 {
						f, err := os.CreateTemp("", "signal-img-*"+pair[1])
						if err == nil {
							_, _ = f.Write(imgData)
							_ = f.Close()
							return "/attach " + f.Name()
						}
					}
				}
			}
		}

		txtOut, err := exec.Command(wlPaste, "--no-newline").Output()
		if err == nil && len(txtOut) > 0 {
			return normalizePastedText(string(bytes.ToValidUTF8(txtOut, []byte("?"))))
		}
		return ""
	}

	for _, cmdArgs := range [][]string{
		{"pbpaste"},
		{"xclip", "-o", "-selection", "clipboard"},
		{"xsel", "-ob"},
	} {
		if bin, err := exec.LookPath(cmdArgs[0]); err == nil {
			out, err := exec.Command(bin, cmdArgs[1:]...).Output()
			if err == nil && len(out) > 0 {
				return normalizePastedText(string(bytes.ToValidUTF8(out, []byte("?"))))
			}
		}
	}
	if runtime.GOOS == "windows" {
		if ps, err := exec.LookPath("powershell"); err == nil {
			out, err := exec.Command(ps, "-NoProfile", "-Command", "Get-Clipboard").Output()
			if err == nil && len(out) > 0 {
				return normalizePastedText(string(bytes.ToValidUTF8(bytes.TrimRight(out, "\r\n"), []byte("?"))))
			}
		}
	}
	return ""
}

// normalizePastedText detects if the pasted text is a single existing file path
// or file:// URI and turns it into `/attach <path>` automatically.
func normalizePastedText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.Contains(trimmed, "\n") {
		return raw
	}
	candidate := trimmed
	if strings.HasPrefix(candidate, "file://") {
		if u, err := url.Parse(candidate); err == nil && u.Path != "" {
			candidate = u.Path
		}
	}
	if strings.HasPrefix(candidate, "'") && strings.HasSuffix(candidate, "'") && len(candidate) > 2 {
		candidate = candidate[1 : len(candidate)-1]
	} else if strings.HasPrefix(candidate, "\"") && strings.HasSuffix(candidate, "\"") && len(candidate) > 2 {
		candidate = candidate[1 : len(candidate)-1]
	}
	if strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "~/") {
		expanded := expandHome(candidate)
		if info, err := os.Stat(expanded); err == nil && !info.IsDir() {
			return "/attach " + quotePathIfNeeded(expanded)
		}
	}
	return raw
}

func quotePathIfNeeded(p string) string {
	if strings.ContainsAny(p, " \t") {
		return "\"" + p + "\""
	}
	return p
}

func CopyAttachment(att Attachment) (bool, string) {
	if att.Path == "" {
		return false, "Attachment path missing"
	}
	info, err := os.Stat(att.Path)
	if err != nil || info.IsDir() {
		return false, fmt.Sprintf("File not found: %s", att.Path)
	}
	name := att.Name
	if name == "" {
		name = filepath.Base(att.Path)
	}

	if strings.HasPrefix(att.ContentType, "image/") {
		if wlCopy, err := exec.LookPath("wl-copy"); err == nil {
			data, err := os.ReadFile(att.Path)
			if err != nil {
				return false, err.Error()
			}
			cmd := exec.Command(wlCopy, "--type", att.ContentType)
			cmd.Stdin = bytes.NewReader(data)
			if err := cmd.Start(); err != nil {
				return false, err.Error()
			}
			go func() { _ = cmd.Wait() }()
			return true, fmt.Sprintf("Copied image: %s", name)
		}
		if xclip, err := exec.LookPath("xclip"); err == nil {
			if err := exec.Command(xclip, "-selection", "clipboard", "-t", att.ContentType, "-i", att.Path).Run(); err != nil {
				return false, err.Error()
			}
			return true, fmt.Sprintf("Copied image: %s", name)
		}
	}

	if wlCopy, err := exec.LookPath("wl-copy"); err == nil {
		cmd := exec.Command(wlCopy)
		cmd.Stdin = strings.NewReader(att.Path)
		_ = cmd.Run()
		return true, fmt.Sprintf("Copied path: %s", att.Path)
	}
	if pbCopy, err := exec.LookPath("pbcopy"); err == nil {
		cmd := exec.Command(pbCopy)
		cmd.Stdin = strings.NewReader(att.Path)
		_ = cmd.Run()
		return true, fmt.Sprintf("Copied path: %s", att.Path)
	}
	if runtime.GOOS == "windows" {
		if ps, err := exec.LookPath("powershell"); err == nil {
			escaped := strings.ReplaceAll(att.Path, "'", "''")
			_ = exec.Command(ps, "-NoProfile", "-Command", "Set-Clipboard -Value '"+escaped+"'").Run()
			return true, fmt.Sprintf("Copied path: %s", att.Path)
		}
	}

	return false, fmt.Sprintf("Saved at: %s", att.Path)
}

func OpenAttachment(att Attachment) (bool, string) {
	if att.Path == "" {
		return false, "Attachment path missing"
	}
	info, err := os.Stat(att.Path)
	if err != nil || info.IsDir() {
		return false, fmt.Sprintf("File not found: %s", att.Path)
	}
	name := att.Name
	if name == "" {
		name = filepath.Base(att.Path)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		opener, err := exec.LookPath("open")
		if err != nil {
			return false, "open not found"
		}
		cmd = exec.Command(opener, att.Path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", att.Path)
	default:
		opener, err := exec.LookPath("xdg-open")
		if err != nil {
			return false, "xdg-open not found"
		}
		cmd = exec.Command(opener, att.Path)
	}
	if err := cmd.Start(); err != nil {
		return false, err.Error()
	}
	go func() { _ = cmd.Wait() }()
	return true, fmt.Sprintf("Opened: %s", name)
}

func NotifyDesktop(title, body string) {
	if bin, err := exec.LookPath("notify-send"); err == nil {
		if len(body) > 180 {
			body = body[:180]
		}
		cmd := exec.Command(bin, "-a", "Signal", title, body)
		_ = cmd.Start()
		go func() { _ = cmd.Wait() }()
	}
}
