# aside

**English** · [한국어](README.ko.md)

Read and send KakaoTalk DMs from your terminal, on a screen that reads as a
coding agent session.

> *aside* — a line delivered on stage for the audience alone, unheard by the
> other characters.

## What it is

aside drives the macOS KakaoTalk app through the system Accessibility API.
There is no server, no protocol reverse engineering, and no message store:
the official app is the only thing that talks to Kakao, and aside reads its
window and types into its composer. Conversations are never written to disk.

- **One Go binary** with the Swift bridge linked in — nothing to install at runtime
- **Stays out of sight** — opens rooms, reads and sends while KakaoTalk is hidden, without stealing focus
- **Korean and English UIs both work** — rooms are matched by structure and geometry, not by label
- **Panic key (F1)** — replaces the whole screen with an English agent session, instantly

## Install

macOS 13 or later (Apple Silicon and Intel).

```bash
make build
./aside doctor   # check accessibility permission and KakaoTalk state
./aside          # run
```

`make dist VERSION=0.1.6` produces a universal binary and installer zip.

**Accessibility permission is required:** System Settings → Privacy & Security
→ Accessibility, and enable the terminal app you run aside from. Quit the
terminal completely (⌘Q) and reopen it afterwards — the permission is not
picked up until then.

## Use

```
↑ / ↓     move between conversations
/         search by name
Enter     open a room / send a message
F1        cover the screen (press again to return)
Esc       back (rooms aside opened are closed again)
?         show shortcuts
Ctrl+Q    quit
```

There are CLI subcommands too:

```bash
aside ls 10             # list conversations (does not mark them read)
aside read 침용          # resolve a partial name and read that room
aside send <room> <text>
aside probe <room>      # diagnose send-button detection (sends nothing)
```

## Worth knowing

- Opening a room marks it **read** in KakaoTalk, exactly as opening it in the
  app would.
- If KakaoTalk is not running, aside launches it hidden, and tells you when a
  login is needed.
- This is not a tool for evading workplace policy or device monitoring. It
  changes how messages are displayed, nothing more; how you use it is on you.

Development notes and the accessibility domain knowledge live in
[CLAUDE.md](CLAUDE.md).
