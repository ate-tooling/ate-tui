# Screenshots for the README. From the repo root:
#   GOOS=linux CGO_ENABLED=0 go build -ldflags "-X main.version=0.1.0" -o bin/ate-tui-linux ./cmd/ate-tui
#   docker run --rm -v "$PWD":/vhs ghcr.io/charmbracelet/vhs docs/tapes/screens.tape
# Each key is on its own line, with a pause before and after each
# screenshot: VHS takes it a moment late, so the next key must wait.
Output bin/screens.gif
Set Shell bash
Set FontSize 15
Set Width 1500
Set Height 860
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 20ms

Type "clear; ./bin/ate-tui-linux --demo --write"
Enter
Sleep 3s
Screenshot docs/images/actors.png
Sleep 1500ms

Type "/crash"
Enter
Sleep 500ms
Enter
Sleep 2s
Screenshot docs/images/explain.png
Sleep 1500ms

Escape
Sleep 300ms
Escape
Sleep 300ms
Type ":pulse"
Enter
Sleep 2s
Screenshot docs/images/pulse.png
Sleep 1500ms

Escape
Sleep 300ms
Type "/nomad"
Enter
Sleep 300ms
Type "l"
Sleep 9s
Screenshot docs/images/logs.png
Sleep 1500ms

Escape
Sleep 300ms
Escape
Sleep 300ms
Type "/research running"
Enter
Sleep 300ms
Type "u"
Sleep 2s
Screenshot docs/images/relations.png
Sleep 1500ms

Escape
Sleep 300ms
# Clear the filter: a filtered row can drop out of the list under the dialog.
Escape
Sleep 300ms
Type "f"
Sleep 1s
Screenshot docs/images/proxy-dialog.png
Sleep 1500ms
Enter
Sleep 1.5s

# quit, so the recording ends
Ctrl+C
Sleep 500ms
