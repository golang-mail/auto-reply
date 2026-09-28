# auto-reply

Vacation / out-of-office responder. For each matching email, sends a reply via the plugin's own SMTP credentials, with a per-sender cooldown so the same person never gets the message more than once per N hours.

Profile selection is **per-filter**: each filter sets `value: <profile-key>` on its `plugin:auto-reply` condition. One profile per identity (work, personal) is the typical setup.

## Install

```bash
go install github.com/golang-mail/auto-reply@latest
```

## Safety rails (auto-skip)

The plugin will NOT reply when:

- `Auto-Submitted` header is set to anything other than `no` — the sender is itself an auto-responder
- `X-Auto-Response-Suppress` is set
- `Precedence: bulk`/`list`/`junk` (mailing lists, marketing)
- `List-Id` or `List-Unsubscribe` header present (mailing lists)
- Sender address contains `noreply@`, `no-reply@`, `mailer-daemon@`, `postmaster@`, or `notifications@github`
- Sender is the same address as the profile's `from` (don't reply to yourself)
- Current time is outside the profile's `active_from` / `active_to` window
- The same sender already got a reply within `cooldown_hours` (default 7 days)

The plugin sets `Auto-Submitted: auto-replied` on its own messages so other systems following these conventions won't loop back.

## Register

```yaml
plugins:
  - name: auto-reply
    command: ["/path/to/auto-reply"]
    transport: socket   # default; the plugin listens on $GO_MAIL_PLUGIN_SOCKET
    timeout: 30s
    config:
      verbose: false
      state_path: "/home/me/.go-mail/auto-reply-state.json"   # cooldown DB
      default_profile: "vacation"
      profiles:
        vacation:
          from: "Me <me@example.com>"
          subject: "Re: {orig_subject}"        # {orig_subject} is replaced
          body: |
            Hi — I'm away from the office until 2026-05-20 and will reply when I return.
            For urgent matters, contact alice@example.com.
          # html: "<p>Hi — I'm away ...</p>"   # optional; if set, sent as multipart/alternative
          smtp_host: "smtp.example.com"
          smtp_port: 587
          smtp_username: "me@example.com"
          smtp_password: "..."
          smtp_tls: "starttls"                 # "tls" (port 465), "starttls" (default), or "plain"
          active_from: "2026-05-06T00:00:00Z"  # RFC3339; empty = always on
          active_to:   "2026-05-20T23:59:59Z"
          cooldown_hours: 168                  # 7 days
        oncall:
          from: "Oncall <oncall@example.com>"
          subject: "Received: {orig_subject}"
          body: "Your message has been received. Expect a response within 4 hours."
          smtp_host: "smtp.example.com"
          smtp_port: 587
          smtp_username: "oncall@example.com"
          smtp_password: "..."
          cooldown_hours: 4
```

## Use it from filters

```yaml
filters:
  - name: vacation-replies
    accounts: ["personal"]
    mailboxes: ["INBOX"]
    conditions:
      - field: plugin:auto-reply
        operator: matches
        value: "vacation"
    actions: []   # the reply IS the action
```

You can pre-filter with cheap conditions to avoid even invoking the plugin on irrelevant mail (it'll skip anyway, but skipping earlier is cheaper):

```yaml
filters:
  - name: oncall-replies
    accounts: ["work"]
    mailboxes: ["INBOX"]
    conditions_type: and
    conditions:
      - field: subject
        operator: contains
        value: "[urgent]"
      - field: plugin:auto-reply
        operator: matches
        value: "oncall"
    actions: []
```

Plugin conditions evaluate last automatically, so the cheap `subject` match short-circuits before the SMTP send is attempted.

## Filter ordering

`auto-reply` declares `run_last: true` in its manifest, so any filter that uses `plugin:auto-reply` is deferred until after all non-plugin filters in the same dependency level have run. That way regular `move`/`delete` rules can clear out auto-generated noise (mailing lists, notifications, bounces) before `auto-reply` is invoked on what remains. Set `run_last: false` in a plugin's manifest if it has no side effects and should compete on specificity like a regular condition.

## Cooldown state

Stored as a JSON map (`profile-key + sender` → unix timestamp) at `state_path`. Survives plugin restarts. To clear it (e.g. you redeployed and want to re-send to recent senders), delete the file. The default location is in `$TMPDIR` — set `state_path` to somewhere persistent for production use.

## Threading

The reply sets `In-Reply-To` and `References` from the original message's `Message-ID`, so most mail clients will thread it under the original conversation.

## Caveats

- The plugin's verdict cache is keyed by `(uid, plugin_version, filter_config)`. If a periodic filter run re-evaluates the same UID, the cache returns the same verdict — which is fine: the reply already happened.
- After config changes (new active window, new body) restart go-mail so the plugin re-reads `config:` and the host's verdict cache is invalidated.
- SMTP credentials live in `plugins.yml` — chmod the file appropriately. A future improvement could read them from env vars or secret files.
