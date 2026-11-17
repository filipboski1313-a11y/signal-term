package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type IncomingEvent struct {
	Kind              string // "message" | "reaction" | "typing" | "receipt" | "delete" | "edit"
	ChatID            string
	ChatType          string // "dm" | "group"
	ChatName          string
	SenderID          string
	SenderNumber      string
	SenderName        string
	Body              string
	Timestamp         int64
	TargetTimestamp   int64
	IsOutgoing        bool
	QuoteAuthor       string
	QuoteText         string
	Attachments       []Attachment
	ReactionEmoji     string
	ReactionTargetTS  int64
	ReactionRemove    bool
	IsTyping          bool
	ReceiptType       string // "delivered" | "read"
	ReceiptTimestamps []int64
	Number            string
	GroupID           string
}

type SendOptions struct {
	Text           string
	Recipient      string
	GroupID        string
	Attachments    []string
	QuoteTimestamp int64
	QuoteAuthor    string
	QuoteMessage   string
	EditTimestamp  int64
}

func signalCLIBin() string {
	if runtime.GOOS == "windows" {
		for _, name := range []string{"signal-cli.bat", "signal-cli.exe", "signal-cli"} {
			if p, err := exec.LookPath(name); err == nil {
				return p
			}
		}
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			for _, candidate := range []string{
				filepath.Join(localAppData, "signal-cli", "bin", "signal-cli.bat"),
				filepath.Join(localAppData, "signal-cli", "bin", "signal-cli.exe"),
			} {
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
		return "signal-cli"
	}
	if p, err := exec.LookPath("signal-cli"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "signal-cli")
}

func signalAttachmentsDir() string {
	if runtime.GOOS == "windows" {
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			winDir := filepath.Join(localAppData, "signal-cli", "attachments")
			if _, err := os.Stat(winDir); err == nil {
				return winDir
			}
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "signal-cli", "attachments")
}

func ListAccounts() []string {
	cmd := exec.Command(signalCLIBin(), "--output=json", "listAccounts")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	var raw []map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil
	}
	var res []string
	for _, item := range raw {
		if num, ok := item["number"].(string); ok && num != "" {
			res = append(res, num)
		}
	}
	return res
}

func LinkDeviceInteractive(deviceName string) bool {
	fmt.Println("\n=== Link Signal Account ===")
	fmt.Println("1. Open Signal on your phone.")
	fmt.Println("2. Go to Settings -> Linked Devices -> + (Link New Device).")
	fmt.Print("3. Scan the QR code below:\n\n")

	cmd := exec.Command(signalCLIBin(), "link", "-n", deviceName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start signal-cli: %v\n", err)
		return false
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start signal-cli: %v\n", err)
		return false
	}

	uri := ""
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "sgnl://") || strings.HasPrefix(line, "tsdevice:/") {
			uri = line
			break
		}
	}

	if uri == "" {
		fmt.Fprintln(os.Stderr, "Failed to obtain QR link URI.")
		_ = cmd.Wait()
		return false
	}

	if qrBin, err := exec.LookPath("qrencode"); err == nil {
		qrCmd := exec.Command(qrBin, "-t", "ANSIUTF8", "-m", "2", uri)
		qrCmd.Stdout = os.Stdout
		qrCmd.Stderr = os.Stderr
		_ = qrCmd.Run()
	} else {
		fmt.Printf("Link URI (scan or install qrencode): %s\n", uri)
	}

	fmt.Println("\nWaiting for scan and device sync...")
	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "Linking failed: %v\n", err)
		return false
	}
	fmt.Print("Linked successfully! Starting terminal chat...\n\n")
	return true
}

func QuickSend(account, recipient, message string) int {
	cmd := exec.Command(signalCLIBin(), "-a", account, "send", "-m", message, recipient)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return 1
	}
	return 0
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type RPCClient struct {
	account  string
	onEvent  func(IncomingEvent)
	onStatus func(string)

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	reqID   int
	pending map[int]chan rpcResult
	closed  bool
}

func NewRPCClient(account string, onEvent func(IncomingEvent), onStatus func(string)) *RPCClient {
	return &RPCClient{
		account:  account,
		onEvent:  onEvent,
		onStatus: onStatus,
		pending:  make(map[int]chan rpcResult),
	}
}

func (c *RPCClient) Start() error {
	c.cmd = exec.Command(signalCLIBin(), "-a", c.account, "jsonRpc")
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := c.cmd.StderrPipe()
	if err != nil {
		return err
	}
	c.stdin = stdin
	if err := c.cmd.Start(); err != nil {
		return err
	}
	go c.readStdout(stdout)
	go c.readStderr(stderr)
	return nil
}

func (c *RPCClient) Stop() {
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- rpcResult{err: errors.New("closed")}
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

func (c *RPCClient) Call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed || c.stdin == nil {
		c.mu.Unlock()
		return nil, errors.New("signal-cli jsonRpc is not running")
	}
	c.reqID++
	id := c.reqID
	ch := make(chan rpcResult, 1)
	c.pending[id] = ch

	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      id,
	}
	if params != nil {
		payload["params"] = params
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	_, writeErr := c.stdin.Write(append(raw, '\n'))
	c.mu.Unlock()

	if writeErr != nil {
		return nil, writeErr
	}

	select {
	case res := <-ch:
		return res.result, res.err
	case <-time.After(25 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, errors.New("rpc timeout")
	}
}

func (c *RPCClient) ListContacts() ([]map[string]any, error) {
	raw, err := c.Call("listContacts", nil)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *RPCClient) ListGroups() ([]map[string]any, error) {
	raw, err := c.Call("listGroups", nil)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *RPCClient) SendMessage(opts SendOptions) (int64, error) {
	params := map[string]any{"message": opts.Text}
	if len(opts.Attachments) > 0 {
		params["attachments"] = opts.Attachments
	}
	if opts.QuoteTimestamp > 0 && opts.QuoteAuthor != "" {
		params["quoteTimestamp"] = opts.QuoteTimestamp
		params["quoteAuthor"] = opts.QuoteAuthor
		if opts.QuoteMessage != "" {
			params["quoteMessage"] = opts.QuoteMessage
		}
	}
	if opts.EditTimestamp > 0 {
		params["editTimestamp"] = opts.EditTimestamp
	}
	if opts.GroupID != "" {
		params["groupId"] = opts.GroupID
	} else if opts.Recipient != "" {
		params["recipient"] = []string{opts.Recipient}
	} else {
		return 0, errors.New("recipient or groupId required")
	}

	raw, err := c.Call("send", params)
	if err != nil {
		return 0, err
	}
	var resMap map[string]any
	if json.Unmarshal(raw, &resMap) == nil {
		if ts := asInt64(resMap["timestamp"]); ts > 0 {
			return ts, nil
		}
	}
	return time.Now().UnixMilli(), nil
}

func (c *RPCClient) SendReaction(recipient, groupID, targetAuthor, emoji string, targetTS int64, remove bool) error {
	params := map[string]any{
		"emoji":           emoji,
		"targetAuthor":    targetAuthor,
		"targetTimestamp": targetTS,
		"remove":          remove,
	}
	if groupID != "" {
		params["groupId"] = groupID
	} else if recipient != "" {
		params["recipient"] = []string{recipient}
	} else {
		return errors.New("recipient or groupId required")
	}
	_, err := c.Call("sendReaction", params)
	return err
}

func (c *RPCClient) SendReadReceipt(recipient string, timestamps []int64) error {
	if recipient == "" || len(timestamps) == 0 {
		return nil
	}
	params := map[string]any{
		"recipient":       recipient,
		"targetTimestamp": timestamps,
		"type":            "read",
	}
	_, err := c.Call("sendReceipt", params)
	return err
}

func (c *RPCClient) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && (strings.Contains(line, "WARN") || strings.Contains(line, "ERROR")) {
			if c.onStatus != nil {
				if len(line) > 70 {
					line = line[:70]
				}
				c.onStatus(line)
			}
		}
	}
}

func (c *RPCClient) readStdout(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if idVal, hasID := msg["id"]; hasID && idVal != nil {
			id := int(asInt64(idVal))
			c.mu.Lock()
			ch, ok := c.pending[id]
			if ok {
				delete(c.pending, id)
			}
			c.mu.Unlock()
			if ok {
				if errObj, hasErr := msg["error"].(map[string]any); hasErr {
					ch <- rpcResult{err: errors.New(asString(errObj["message"]))}
				} else {
					rawRes, _ := json.Marshal(msg["result"])
					ch <- rpcResult{result: rawRes}
				}
			}
			continue
		}
		if asString(msg["method"]) == "receive" {
			if params, ok := msg["params"].(map[string]any); ok {
				if env, ok := params["envelope"].(map[string]any); ok {
					c.handleEnvelope(env)
				}
			}
		}
	}
}

func (c *RPCClient) handleEnvelope(env map[string]any) {
	sourceUUID := asString(env["sourceUuid"])
	sourceNum := asString(env["sourceNumber"])
	sourceName := asString(env["sourceName"])
	if sourceName == "" {
		sourceName = sourceNum
	}
	if sourceName == "" {
		sourceName = sourceUUID
	}
	if sourceName == "" {
		sourceName = "Unknown"
	}
	sourceID := sourceUUID
	if sourceID == "" {
		sourceID = sourceNum
	}
	if sourceID == "" {
		sourceID = "unknown"
	}
	ts := asInt64(env["timestamp"])

	// 1. Typing indicator
	if typing, ok := env["typingMessage"].(map[string]any); ok {
		gid := asString(typing["groupId"])
		chatID := sourceID
		chatType := "dm"
		if gid != "" {
			chatID = "group:" + gid
			chatType = "group"
		}
		c.onEvent(IncomingEvent{
			Kind:       "typing",
			ChatID:     chatID,
			ChatType:   chatType,
			ChatName:   sourceName,
			SenderID:   sourceID,
			SenderName: sourceName,
			Number:     sourceNum,
			Timestamp:  ts,
			IsTyping:   asString(typing["action"]) == "STARTED",
		})
		return
	}

	// 2. Read & delivery receipts
	if receipt, ok := env["receiptMessage"].(map[string]any); ok {
		isRead, _ := receipt["isRead"].(bool)
		isDelivery, _ := receipt["isDelivery"].(bool)
		receiptType := ""
		if isRead {
			receiptType = "read"
		} else if isDelivery {
			receiptType = "delivered"
		}
		if receiptType != "" {
			var timestamps []int64
			if rawTS, ok := receipt["timestamps"].([]any); ok {
				for _, item := range rawTS {
					if v := asInt64(item); v > 0 {
						timestamps = append(timestamps, v)
					}
				}
			}
			if len(timestamps) > 0 {
				c.onEvent(IncomingEvent{
					Kind:              "receipt",
					SenderID:          sourceID,
					SenderNumber:      sourceNum,
					ReceiptType:       receiptType,
					ReceiptTimestamps: timestamps,
				})
			}
		}
		return
	}

	// 3. Edit message
	if editMsg, ok := env["editMessage"].(map[string]any); ok {
		targetTS := asInt64(editMsg["targetSentTimestamp"])
		if dataMsg, ok := editMsg["dataMessage"].(map[string]any); ok && targetTS > 0 {
			chatID, chatType, chatName, num, gid := resolveTargetFromDataMsg(dataMsg, sourceID, sourceName, sourceNum)
			c.onEvent(IncomingEvent{
				Kind:            "edit",
				ChatID:          chatID,
				ChatType:        chatType,
				ChatName:        chatName,
				SenderID:        sourceID,
				SenderNumber:    sourceNum,
				SenderName:      sourceName,
				Body:            asString(dataMsg["message"]),
				TargetTimestamp: targetTS,
				Timestamp:       ts,
				Number:          num,
				GroupID:         gid,
			})
		}
		return
	}

	// 4. Incoming dataMessage
	if dataMsg, ok := env["dataMessage"].(map[string]any); ok {
		c.emitDataMessage(dataMsg, sourceID, sourceNum, sourceName, sourceID, sourceName, sourceNum, false, ts)
		return
	}

	// 5. Synced outgoing message
	if syncMsg, ok := env["syncMessage"].(map[string]any); ok {
		if sent, ok := syncMsg["sentMessage"].(map[string]any); ok {
			destUUID := asString(sent["destinationUuid"])
			destNum := asString(sent["destinationNumber"])
			destID := destUUID
			if destID == "" {
				destID = destNum
			}
			if destID == "" {
				destID = c.account
			}
			destName := destNum
			if destName == "" {
				destName = destUUID
			}
			if destName == "" || destNum == c.account {
				destName = "Note to Self"
			}
			sts := asInt64(sent["timestamp"])
			if sts == 0 {
				sts = ts
			}
			if editMsg, ok := sent["editMessage"].(map[string]any); ok {
				targetTS := asInt64(editMsg["targetSentTimestamp"])
				if dataMsg, ok := editMsg["dataMessage"].(map[string]any); ok && targetTS > 0 {
					chatID, chatType, chatName, num, gid := resolveTargetFromDataMsg(dataMsg, destID, destName, destNum)
					c.onEvent(IncomingEvent{
						Kind:            "edit",
						ChatID:          chatID,
						ChatType:        chatType,
						ChatName:        chatName,
						SenderID:        c.account,
						SenderName:      "You",
						Body:            asString(dataMsg["message"]),
						TargetTimestamp: targetTS,
						Timestamp:       sts,
						Number:          num,
						GroupID:         gid,
					})
					return
				}
			}
			c.emitDataMessage(sent, c.account, c.account, "You", destID, destName, destNum, true, sts)
		}
	}
}

func resolveTargetFromDataMsg(dataMsg map[string]any, fallbackID, fallbackName, fallbackNum string) (string, string, string, string, string) {
	if groupInfo, ok := dataMsg["groupInfo"].(map[string]any); ok {
		if gid := asString(groupInfo["groupId"]); gid != "" {
			gname := asString(groupInfo["groupName"])
			if gname == "" {
				gname = gid
			}
			return "group:" + gid, "group", gname, "", gid
		}
	}
	return fallbackID, "dm", fallbackName, fallbackNum, ""
}

func (c *RPCClient) emitDataMessage(
	dataMsg map[string]any,
	senderID, senderNum, senderName, fallbackChatID, fallbackChatName, fallbackNum string,
	isOutgoing bool,
	envTS int64,
) {
	ts := asInt64(dataMsg["timestamp"])
	if ts == 0 {
		ts = envTS
	}

	chatID, chatType, chatName, number, gid := resolveTargetFromDataMsg(dataMsg, fallbackChatID, fallbackChatName, fallbackNum)

	// Remote delete
	if remDel, ok := dataMsg["remoteDelete"].(map[string]any); ok {
		if targetTS := asInt64(remDel["timestamp"]); targetTS > 0 {
			c.onEvent(IncomingEvent{
				Kind:            "delete",
				ChatID:          chatID,
				ChatType:        chatType,
				ChatName:        chatName,
				TargetTimestamp: targetTS,
				Number:          number,
				GroupID:         gid,
			})
			return
		}
	}

	if reaction, ok := dataMsg["reaction"].(map[string]any); ok {
		if emoji := asString(reaction["emoji"]); emoji != "" {
			isRemove, _ := reaction["isRemove"].(bool)
			c.onEvent(IncomingEvent{
				Kind:             "reaction",
				ChatID:           chatID,
				ChatType:         chatType,
				ChatName:         chatName,
				SenderID:         senderID,
				SenderNumber:     senderNum,
				SenderName:       senderName,
				Timestamp:        ts,
				ReactionEmoji:    emoji,
				ReactionTargetTS: asInt64(reaction["targetSentTimestamp"]),
				ReactionRemove:   isRemove,
				Number:           number,
				GroupID:          gid,
			})
			return
		}
	}

	body := asString(dataMsg["message"])
	var attachments []Attachment
	if rawAtts, ok := dataMsg["attachments"].([]any); ok {
		attDir := signalAttachmentsDir()
		for _, item := range rawAtts {
			att, ok := item.(map[string]any)
			if !ok {
				continue
			}
			attID := asString(att["id"])
			ctype := asString(att["contentType"])
			if ctype == "" {
				ctype = "application/octet-stream"
			}
			fname := asString(att["filename"])
			if fname == "" {
				fname = attID
			}
			if fname == "" {
				fname = ctype
			}
			fpath := asString(att["file"])
			if fpath == "" && attID != "" {
				fpath = filepath.Join(attDir, attID)
			}
			attachments = append(attachments, Attachment{
				Name:        fname,
				Path:        fpath,
				ContentType: ctype,
			})
		}
	}

	if body == "" && len(attachments) == 0 {
		return
	}

	quoteAuthor := ""
	quoteText := ""
	if quote, ok := dataMsg["quote"].(map[string]any); ok {
		quoteAuthor = asString(quote["authorNumber"])
		if quoteAuthor == "" {
			quoteAuthor = asString(quote["authorUuid"])
		}
		quoteText = asString(quote["text"])
	}

	c.onEvent(IncomingEvent{
		Kind:         "message",
		ChatID:       chatID,
		ChatType:     chatType,
		ChatName:     chatName,
		SenderID:     senderID,
		SenderNumber: senderNum,
		SenderName:   senderName,
		Body:         body,
		Timestamp:    ts,
		IsOutgoing:   isOutgoing,
		QuoteAuthor:  quoteAuthor,
		QuoteText:    quoteText,
		Attachments:  attachments,
		Number:       number,
		GroupID:      gid,
	})
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt64(v any) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case int64:
		return val
	case int:
		return int64(val)
	case json.Number:
		i, _ := val.Int64()
		return i
	}
	return 0
}
