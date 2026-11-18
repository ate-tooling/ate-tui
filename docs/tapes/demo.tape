# The tour GIF at the top of the README. From the repo root:
#   GOOS=linux CGO_ENABLED=0 go build -ldflags "-X main.version=0.1.0" -o bin/ate-tui-linux ./cmd/ate-tui
#   docker run --rm -v "$PWD":/vhs ghcr.io/charmbracelet/vhs docs/tapes/demo.tape
Output docs/images/demo.gif
Set Shell bash
Set FontSize 14
Set Width 1200
Set Height 700
Set Padding 14
Set Framerate 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 90ms

Hide
Type "clear; ./bin/ate-tui-linux --demo --write"
Enter
Sleep 2.5s
Show
Sleep 2s

# move, then filter to the crashed actor and explain it
Type "jjj"
Sleep 600ms
Type "/crash"
Sleep 600ms
Enter
Sleep 600ms
Enter
Sleep 3s

# cluster overview
Escape
Sleep 400ms
Escape
Sleep 400ms
Type ":pulse"
Sleep 400ms
Enter
Sleep 3s

# workers, then relations of one
Escape
Sleep 400ms
Type ":workers"
Sleep 400ms
Enter
Sleep 1.5s
Type "u"
Sleep 2.5s

# follow the actor that moves between workers
Escape
Sleep 400ms
Type ":actors"
Enter
Sleep 600ms
Type "/nomad"
Enter
Sleep 400ms
Type "l"
Sleep 8s

# quit, so the recording ends
Ctrl+C
Sleep 500ms
