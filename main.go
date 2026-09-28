// auto-reply sends a vacation / out-of-office reply to incoming mail.
//
// Profile selection is per-filter: filter conditions set `value: <profile-key>`
// where the key is one of the names declared in this plugin's config. Each
// profile carries its own SMTP credentials, From address, message body, and
// optional active window.
//
// Safety rails:
//   - skips list / bulk / auto-generated mail
//   - skips known no-reply / mailer-daemon senders
//   - per-sender cooldown (default 7 days) persisted to disk so restarts don't
//     trigger floods
//   - sets Auto-Submitted: auto-replied so other auto-responders won't loop
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type envelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type initRequest struct {
	HostVersion string          `json:"host_version"`
	Config      json.RawMessage `json:"config,omitempty"`
}

// profile is one auto-reply identity: SMTP creds + message + active window.
type profile struct {
	From    string `yaml:"from" json:"from"`       // "Name <user@example.com>" or just an address
	Subject string `yaml:"subject" json:"subject"` // {orig_subject} is replaced with the original Subject
	Body    string `yaml:"body" json:"body"`       // plain-text body
	HTML    string `yaml:"html" json:"html"`       // optional HTML body; if both present, multipart/alternative

	// SMTP
	SMTPHost     string `yaml:"smtp_host" json:"smtp_host"`
	SMTPPort     int    `yaml:"smtp_port" json:"smtp_port"`
	SMTPUsername string `yaml:"smtp_username" json:"smtp_username"`
	SMTPPassword string `yaml:"smtp_password" json:"smtp_password"`
	SMTPTLS      string `yaml:"smtp_tls" json:"smtp_tls"` // "tls", "starttls", "plain"; default "starttls"

	// Active window (RFC3339 dates). Empty = always active.
	ActiveFrom string `yaml:"active_from" json:"active_from"`
	ActiveTo   string `yaml:"active_to" json:"active_to"`

	// CooldownHours: don't reply to the same sender more than once per this window.
	CooldownHours int `yaml:"cooldown_hours" json:"cooldown_hours"`
}

type pluginConfig struct {
	DefaultProfile string             `yaml:"default_profile" json:"default_profile"`
	Profiles       map[string]profile `yaml:"profiles" json:"profiles"`
	// StatePath is where per-sender cooldown state is persisted.
	StatePath string `yaml:"state_path" json:"state_path"`
	Verbose   bool   `yaml:"verbose" json:"verbose"`
}

type address struct {
	PersonalName string `json:"PersonalName"`
	MailboxName  string `json:"MailboxName"`
	HostName     string `json:"HostName"`
}

type emailEnvelope struct {
	Date      time.Time  `json:"Date"`
	Subject   string     `json:"Subject"`
	MessageID string     `json:"MessageId"`
	From      []*address `json:"From"`
	To        []*address `json:"To"`
	Cc        []*address `json:"Cc"`
}

type evalReq struct {
	Email struct {
		UID      uint32            `json:"uid"`
		Account  string            `json:"account"`
		Mailbox  string            `json:"mailbox"`
		Envelope *emailEnvelope    `json:"envelope"`
		Headers  map[string]string `json:"headers"`
	} `json:"email"`
	FilterConfig string `json:"filter_config"`
}

var (
	cfg     pluginConfig
	stateMu sync.Mutex
	state   = map[string]int64{} // sender-email → unix seconds of last reply
)

func main() {
	log.SetFlags(0)
	path := os.Getenv("GO_MAIL_PLUGIN_SOCKET")
	if path == "" {
		log.Fatal("GO_MAIL_PLUGIN_SOCKET not set")
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		log.Fatalf("listen %s: %v", path, err)
	}
	_ = os.Chmod(path, 0o600)
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serve(conn)
	}
}

func serve(conn net.Conn) {
	defer conn.Close()
	in := bufio.NewScanner(conn)
	in.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	out := json.NewEncoder(conn)

	for in.Scan() {
		var msg envelope
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			log.Printf("bad envelope: %v", err)
			continue
		}
		switch msg.Method {
		case "init":
			handleInit(out, msg)
		case "evaluate":
			handleEvaluate(out, msg)
		case "shutdown":
			saveState()
			return
		}
	}
}

func handleInit(out *json.Encoder, msg envelope) {
	var req initRequest
	_ = json.Unmarshal(msg.Payload, &req)
	if len(req.Config) > 0 {
		if err := json.Unmarshal(req.Config, &cfg); err != nil {
			log.Printf("decode plugin config: %v", err)
		}
	}
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(os.TempDir(), "go-mail-auto-reply-state.json")
	}
	loadState()

	manifest, _ := json.Marshal(map[string]any{
		"name":      "auto-reply",
		"version":   "0.1.0",
		"actions":   []string{},
		"wants":     []string{"envelope", "headers"},
		"cacheable": true, // verdict per (uid, profile) is deterministic for the lifetime of a run
		"run_last":  true, // wait for cleanup filters before sending replies
	})
	_ = out.Encode(envelope{
		ID:      msg.ID,
		Type:    "response",
		Method:  "init",
		Payload: manifest,
	})
	log.Printf("initialized: %d profiles configured (state=%s)", len(cfg.Profiles), cfg.StatePath)
}

func handleEvaluate(out *json.Encoder, msg envelope) {
	var r evalReq
	if err := json.Unmarshal(msg.Payload, &r); err != nil {
		respond(out, msg.ID, map[string]any{"error": "decode evaluate: " + err.Error()})
		return
	}

	prof, key, ok := pickProfile(r.FilterConfig)
	if !ok {
		log.Printf("no profile resolved (filter_config=%q, default=%q) — skipping", r.FilterConfig, cfg.DefaultProfile)
		respond(out, msg.ID, map[string]any{})
		return
	}

	if reason, skip := shouldSkip(&r, prof); skip {
		if cfg.Verbose {
			log.Printf("skip uid=%d profile=%s reason=%s", r.Email.UID, key, reason)
		}
		respond(out, msg.ID, map[string]any{"reason": "skipped: " + reason})
		return
	}

	senderEmail := senderAddress(r.Email.Envelope)
	if senderEmail == "" {
		respond(out, msg.ID, map[string]any{"reason": "no sender address"})
		return
	}

	// Cooldown gate.
	cooldown := time.Duration(prof.CooldownHours) * time.Hour
	if cooldown == 0 {
		cooldown = 7 * 24 * time.Hour
	}
	cooldownKey := key + "\x00" + strings.ToLower(senderEmail)
	stateMu.Lock()
	last, seen := state[cooldownKey]
	stateMu.Unlock()
	if seen && time.Since(time.Unix(last, 0)) < cooldown {
		if cfg.Verbose {
			log.Printf("cooldown uid=%d sender=%s last=%s", r.Email.UID, senderEmail, time.Unix(last, 0).Format(time.RFC3339))
		}
		respond(out, msg.ID, map[string]any{"reason": "cooldown"})
		return
	}

	if err := sendReply(prof, &r, senderEmail); err != nil {
		log.Printf("send auto-reply: %v", err)
		respond(out, msg.ID, map[string]any{"error": err.Error()})
		return
	}

	stateMu.Lock()
	state[cooldownKey] = time.Now().Unix()
	stateMu.Unlock()
	saveState()

	respond(out, msg.ID, map[string]any{"matched": true, "reason": "auto-replied to " + senderEmail})
}

func pickProfile(key string) (profile, string, bool) {
	tryKey := func(k string) (profile, bool) {
		if k == "" {
			return profile{}, false
		}
		p, found := cfg.Profiles[k]
		return p, found
	}
	if p, ok := tryKey(key); ok {
		return p, key, true
	}
	if p, ok := tryKey(cfg.DefaultProfile); ok {
		return p, cfg.DefaultProfile, true
	}
	return profile{}, "", false
}

// shouldSkip returns (reason, true) when we must NOT auto-reply.
func shouldSkip(r *evalReq, prof profile) (string, bool) {
	// Active window check.
	now := time.Now()
	if prof.ActiveFrom != "" {
		if t, err := time.Parse(time.RFC3339, prof.ActiveFrom); err == nil && now.Before(t) {
			return "before active_from", true
		}
	}
	if prof.ActiveTo != "" {
		if t, err := time.Parse(time.RFC3339, prof.ActiveTo); err == nil && now.After(t) {
			return "after active_to", true
		}
	}

	get := func(name string) string {
		for k, v := range r.Email.Headers {
			if strings.EqualFold(k, name) {
				return v
			}
		}
		return ""
	}

	// Don't reply to mail flagged as auto-generated/auto-replied.
	if v := strings.ToLower(get("Auto-Submitted")); v != "" && v != "no" {
		return "Auto-Submitted=" + v, true
	}
	if v := get("X-Auto-Response-Suppress"); v != "" {
		return "X-Auto-Response-Suppress set", true
	}
	// Don't reply to mailing lists / bulk.
	if v := strings.ToLower(get("Precedence")); v == "bulk" || v == "list" || v == "junk" {
		return "Precedence=" + v, true
	}
	if get("List-Id") != "" || get("List-Unsubscribe") != "" {
		return "list-mail", true
	}

	// Don't reply to no-reply senders.
	from := senderAddress(r.Email.Envelope)
	low := strings.ToLower(from)
	for _, p := range []string{"noreply@", "no-reply@", "donotreply@", "do-not-reply@", "mailer-daemon@", "postmaster@", "bounce", "notifications@github"} {
		if strings.Contains(low, p) {
			return "no-reply sender: " + p, true
		}
	}

	// Don't reply to ourselves.
	if from != "" && profileFromAddress(prof) != "" && strings.EqualFold(from, profileFromAddress(prof)) {
		return "from self", true
	}

	return "", false
}

// profileFromAddress returns the bare email address part of profile.From.
func profileFromAddress(p profile) string {
	s := p.From
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j >= 0 {
			return strings.TrimSpace(s[i+1 : i+j])
		}
	}
	return strings.TrimSpace(s)
}

func senderAddress(e *emailEnvelope) string {
	if e == nil || len(e.From) == 0 {
		return ""
	}
	a := e.From[0]
	if a.MailboxName == "" || a.HostName == "" {
		return ""
	}
	return a.MailboxName + "@" + a.HostName
}

func sendReply(p profile, r *evalReq, to string) error {
	if p.SMTPHost == "" || p.From == "" {
		return fmt.Errorf("profile incomplete: smtp_host or from missing")
	}

	subject := p.Subject
	origSubject := ""
	if r.Email.Envelope != nil {
		origSubject = r.Email.Envelope.Subject
	}
	if subject == "" {
		subject = "Re: " + origSubject
	} else {
		subject = strings.ReplaceAll(subject, "{orig_subject}", origSubject)
	}

	msgID := getHeader(r.Email.Headers, "Message-ID")
	if msgID == "" && r.Email.Envelope != nil {
		msgID = r.Email.Envelope.MessageID
	}
	references := getHeader(r.Email.Headers, "References")

	mime := buildMIME(p, to, subject, msgID, references)

	addr := fmt.Sprintf("%s:%d", p.SMTPHost, smtpPort(p))
	mode := strings.ToLower(p.SMTPTLS)
	if mode == "" {
		mode = "starttls"
	}

	var auth smtp.Auth
	if p.SMTPUsername != "" {
		auth = smtp.PlainAuth("", p.SMTPUsername, p.SMTPPassword, p.SMTPHost)
	}

	from := profileFromAddress(p)
	switch mode {
	case "tls":
		return sendMailTLS(addr, p.SMTPHost, auth, from, []string{to}, mime)
	case "starttls", "plain":
		// smtp.SendMail does STARTTLS opportunistically when the server advertises it.
		return smtp.SendMail(addr, auth, from, []string{to}, mime)
	default:
		return fmt.Errorf("unknown smtp_tls mode %q", mode)
	}
}

func smtpPort(p profile) int {
	if p.SMTPPort > 0 {
		return p.SMTPPort
	}
	switch strings.ToLower(p.SMTPTLS) {
	case "tls":
		return 465
	default:
		return 587
	}
}

func sendMailTLS(addr, host string, auth smtp.Auth, from string, to []string, msg []byte) error {
	tlsCfg := &tls.Config{ServerName: host}
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("tls dial: %w", err)
	}
	defer conn.Close()
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp new client: %w", err)
	}
	defer c.Quit()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("rcpt: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return w.Close()
}

func buildMIME(p profile, to, subject, inReplyTo, references string) []byte {
	var b strings.Builder
	b.WriteString("From: " + p.From + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("X-Auto-Response-Suppress: All\r\n") // discourage other clients from auto-replying back
	b.WriteString("Precedence: auto_reply\r\n")
	if inReplyTo != "" {
		b.WriteString("In-Reply-To: " + inReplyTo + "\r\n")
		if references != "" {
			b.WriteString("References: " + references + " " + inReplyTo + "\r\n")
		} else {
			b.WriteString("References: " + inReplyTo + "\r\n")
		}
	}
	b.WriteString("MIME-Version: 1.0\r\n")

	switch {
	case p.HTML != "" && p.Body != "":
		boundary := "----=auto-reply-" + fmt.Sprintf("%d", time.Now().UnixNano())
		b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
		b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
		b.WriteString(p.Body)
		b.WriteString("\r\n--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\n\r\n")
		b.WriteString(p.HTML)
		b.WriteString("\r\n--" + boundary + "--\r\n")
	case p.HTML != "":
		b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		b.WriteString(p.HTML)
	default:
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		b.WriteString(p.Body)
	}
	return []byte(b.String())
}

func getHeader(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func loadState() {
	stateMu.Lock()
	defer stateMu.Unlock()
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		return
	}
	loaded := map[string]int64{}
	if err := json.Unmarshal(data, &loaded); err == nil {
		state = loaded
		log.Printf("loaded %d cooldown entries from %s", len(state), cfg.StatePath)
	}
}

func saveState() {
	stateMu.Lock()
	data, err := json.Marshal(state)
	stateMu.Unlock()
	if err != nil {
		log.Printf("marshal state: %v", err)
		return
	}
	tmp := cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("write state: %v", err)
		return
	}
	if err := os.Rename(tmp, cfg.StatePath); err != nil {
		log.Printf("rename state: %v", err)
	}
}

func respond(out *json.Encoder, id string, payload any) {
	raw, _ := json.Marshal(payload)
	_ = out.Encode(envelope{
		ID:      id,
		Type:    "response",
		Method:  "evaluate",
		Payload: raw,
	})
}
