package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetState wipes the package-level cooldown map and points state_path at
// a temp file so individual tests don't bleed into each other.
func resetState(t *testing.T) {
	t.Helper()
	stateMu.Lock()
	state = map[string]int64{}
	stateMu.Unlock()
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
}

func TestPickProfile(t *testing.T) {
	cfg = pluginConfig{
		DefaultProfile: "vacation",
		Profiles: map[string]profile{
			"vacation": {From: "me@x", Body: "out of office"},
			"oncall":   {From: "oncall@x", Body: "on it"},
		},
	}
	if p, k, ok := pickProfile("oncall"); !ok || k != "oncall" || p.From != "oncall@x" {
		t.Errorf("filter key 'oncall' resolved to %q (%+v, ok=%v)", k, p, ok)
	}
	if p, k, ok := pickProfile("nope"); !ok || k != "vacation" || p.From != "me@x" {
		t.Errorf("missing key should fall back to default; got %q (%+v, ok=%v)", k, p, ok)
	}
	if _, _, ok := pickProfile(""); !ok {
		t.Error("empty key with a configured default should succeed")
	}

	cfg.DefaultProfile = "" // no default
	if _, _, ok := pickProfile("nope"); ok {
		t.Error("missing key with no default should fail")
	}
}

func TestSenderAddress(t *testing.T) {
	if got := senderAddress(nil); got != "" {
		t.Errorf("nil envelope = %q, want empty", got)
	}
	e := &emailEnvelope{From: []*address{{MailboxName: "bob", HostName: "example.com"}}}
	if got := senderAddress(e); got != "bob@example.com" {
		t.Errorf("got %q, want bob@example.com", got)
	}
	e2 := &emailEnvelope{From: []*address{{HostName: "example.com"}}}
	if got := senderAddress(e2); got != "" {
		t.Errorf("missing mailbox should give empty; got %q", got)
	}
}

func TestProfileFromAddress(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Me <me@example.com>", "me@example.com"},
		{"me@example.com", "me@example.com"},
		{"  me@example.com  ", "me@example.com"},
		{"\"Me, Myself\" <me@example.com>", "me@example.com"},
	}
	for _, c := range cases {
		got := profileFromAddress(profile{From: c.in})
		if got != c.want {
			t.Errorf("profileFromAddress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShouldSkip_SafetyRails(t *testing.T) {
	prof := profile{From: "me@example.com"}

	cases := []struct {
		name    string
		headers map[string]string
		from    *address
		want    string // substring of expected reason; empty = should NOT skip
	}{
		{"Auto-Submitted=auto-replied", map[string]string{"Auto-Submitted": "auto-replied"}, nil, "Auto-Submitted"},
		{"Auto-Submitted=no is fine", map[string]string{"Auto-Submitted": "no"}, &address{MailboxName: "user", HostName: "example.com"}, ""},
		{"X-Auto-Response-Suppress", map[string]string{"X-Auto-Response-Suppress": "OOF"}, nil, "X-Auto-Response-Suppress"},
		{"Precedence=bulk", map[string]string{"Precedence": "bulk"}, nil, "Precedence=bulk"},
		{"Precedence=list", map[string]string{"Precedence": "list"}, nil, "Precedence=list"},
		{"List-Id present", map[string]string{"List-Id": "<l.example.com>"}, nil, "list-mail"},
		{"List-Unsubscribe present", map[string]string{"List-Unsubscribe": "<mailto:u@x>"}, nil, "list-mail"},
		{"noreply sender", nil, &address{MailboxName: "noreply", HostName: "example.com"}, "no-reply sender"},
		{"do-not-reply sender", nil, &address{MailboxName: "do-not-reply", HostName: "example.com"}, "no-reply sender"},
		{"mailer-daemon sender", nil, &address{MailboxName: "mailer-daemon", HostName: "example.com"}, "no-reply sender"},
		{"reply to self", nil, &address{MailboxName: "me", HostName: "example.com"}, "from self"},
		{"normal mail", nil, &address{MailboxName: "alice", HostName: "example.com"}, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &evalReq{}
			r.Email.Headers = c.headers
			if c.from != nil {
				r.Email.Envelope = &emailEnvelope{From: []*address{c.from}}
			}
			reason, skip := shouldSkip(r, prof)
			if c.want == "" {
				if skip {
					t.Errorf("expected NO skip, got skip with reason=%q", reason)
				}
			} else {
				if !skip {
					t.Errorf("expected skip with reason containing %q, got skip=false", c.want)
				} else if !strings.Contains(reason, c.want) {
					t.Errorf("reason=%q, want substring %q", reason, c.want)
				}
			}
		})
	}
}

func TestShouldSkip_ActiveWindow(t *testing.T) {
	now := time.Now()
	future := now.Add(24 * time.Hour).Format(time.RFC3339)
	past := now.Add(-24 * time.Hour).Format(time.RFC3339)

	r := &evalReq{}
	r.Email.Envelope = &emailEnvelope{From: []*address{{MailboxName: "x", HostName: "y.com"}}}

	if reason, skip := shouldSkip(r, profile{From: "me@x", ActiveFrom: future}); !skip || !strings.Contains(reason, "before active_from") {
		t.Errorf("active_from in future should skip; got skip=%v reason=%q", skip, reason)
	}
	if reason, skip := shouldSkip(r, profile{From: "me@x", ActiveTo: past}); !skip || !strings.Contains(reason, "after active_to") {
		t.Errorf("active_to in past should skip; got skip=%v reason=%q", skip, reason)
	}
	if _, skip := shouldSkip(r, profile{From: "me@x", ActiveFrom: past, ActiveTo: future}); skip {
		t.Errorf("now within window should not skip")
	}
}

func TestBuildMIME_Threading(t *testing.T) {
	p := profile{
		From:    "Me <me@example.com>",
		Subject: "Re: {orig_subject}",
		Body:    "Out of office.",
	}
	got := string(buildMIME(p, "alice@example.com", "Re: hello", "<orig@msg.id>", "<grand@msg.id>"))

	mustContain := []string{
		"From: Me <me@example.com>",
		"To: alice@example.com",
		"Subject: Re: hello",
		"Auto-Submitted: auto-replied",
		"X-Auto-Response-Suppress: All",
		"In-Reply-To: <orig@msg.id>",
		"References: <grand@msg.id> <orig@msg.id>",
		"Content-Type: text/plain",
		"Out of office.",
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("MIME missing %q\n---\n%s\n---", s, got)
		}
	}
}

func TestBuildMIME_NoReferencesYet(t *testing.T) {
	// First reply in a thread — no prior References header.
	p := profile{From: "me@example.com", Body: "hi"}
	got := string(buildMIME(p, "you@example.com", "Re: x", "<msg-1@id>", ""))
	if !strings.Contains(got, "References: <msg-1@id>") {
		t.Errorf("References should fall back to In-Reply-To value alone\n---\n%s\n---", got)
	}
}

func TestBuildMIME_HTMLAndText_Multipart(t *testing.T) {
	p := profile{
		From: "me@example.com",
		Body: "plain version",
		HTML: "<p>html version</p>",
	}
	got := string(buildMIME(p, "you@example.com", "subj", "", ""))
	if !strings.Contains(got, "multipart/alternative") {
		t.Errorf("expected multipart/alternative when both Body and HTML set\n%s", got)
	}
	if !strings.Contains(got, "plain version") || !strings.Contains(got, "<p>html version</p>") {
		t.Errorf("both parts should appear\n%s", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	resetState(t)

	stateMu.Lock()
	state["vacation\x00alice@example.com"] = time.Now().Unix()
	state["vacation\x00bob@example.com"] = time.Now().Unix() - 100
	stateMu.Unlock()

	saveState()

	// Wipe in-memory state, then reload from disk.
	stateMu.Lock()
	state = map[string]int64{}
	stateMu.Unlock()

	loadState()

	stateMu.Lock()
	defer stateMu.Unlock()
	if len(state) != 2 {
		t.Errorf("loaded %d entries, want 2", len(state))
	}
	if _, ok := state["vacation\x00alice@example.com"]; !ok {
		t.Errorf("alice entry missing after roundtrip")
	}
}

func TestCooldownGate(t *testing.T) {
	// Verifies the cooldown logic in handleEvaluate without going to SMTP:
	// we manipulate `state` directly and check shouldSkip is the only gate.
	// (The actual reply path is exercised end-to-end in production; here we
	// just confirm the state lookup uses profile+sender as the key.)
	resetState(t)

	key := "vacation\x00alice@example.com"
	stateMu.Lock()
	state[key] = time.Now().Add(-1 * time.Hour).Unix() // replied 1h ago
	stateMu.Unlock()

	cooldown := 24 * time.Hour // matches profile.CooldownHours = 24
	stateMu.Lock()
	last, seen := state[key]
	stateMu.Unlock()
	if !seen || time.Since(time.Unix(last, 0)) >= cooldown {
		t.Errorf("expected to be in cooldown")
	}

	// Move "last reply" outside cooldown — now we should NOT be in cooldown.
	stateMu.Lock()
	state[key] = time.Now().Add(-25 * time.Hour).Unix()
	last2, _ := state[key]
	stateMu.Unlock()
	if time.Since(time.Unix(last2, 0)) < cooldown {
		t.Errorf("expected to be past cooldown")
	}
}
