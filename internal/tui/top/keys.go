package top

import "charm.land/bubbles/v2/key"

type keymap struct {
	Left, Right, Up, Down key.Binding
	Submit, Cancel        key.Binding
	SubmitWindow          key.Binding
	Help, Quit            key.Binding
	Refresh               key.Binding

	// Contextual: repos column.
	AddRepo, RemoveRepo key.Binding

	// Contextual: containers column.
	NewContainer, RemoveContainer key.Binding

	// Contextual: details pane (when a worktree is selected).
	ToggleExpose, ToggleMirror key.Binding
	Forward                    key.Binding

	// Contextual: when a container is selected (in the containers column
	// or in the details pane showing that container's info).
	CopyClaudeCmd, CopyShellCmd key.Binding
	StartStop                   key.Binding
	OpenIDE                     key.Binding
}

func newKeymap() keymap {
	return keymap{
		Left:         key.NewBinding(key.WithKeys("left"), key.WithHelp("←↓↑→", "")),
		Right:        key.NewBinding(key.WithKeys("right")),
		Up:           key.NewBinding(key.WithKeys("up")),
		Down:         key.NewBinding(key.WithKeys("down")),
		Submit:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "submit")),
		SubmitWindow: key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "new window")),
		Cancel:       key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Help:         key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:         key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Refresh:      key.NewBinding(key.WithKeys("f5"), key.WithHelp("F5", "refresh")),

		AddRepo:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add repo")),
		RemoveRepo: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove repo")),

		NewContainer:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "create container")),
		RemoveContainer: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove container")),

		ToggleExpose: key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "toggle expose")),
		ToggleMirror: key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "toggle mirror")),
		Forward:      key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "forward host port")),

		CopyClaudeCmd: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "run `ahjo claude`")),
		CopyShellCmd:  key.NewBinding(key.WithKeys("h"), key.WithHelp("h", "run `ahjo shell`")),
		StartStop:     key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start/stop")),
		OpenIDE:       key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "ide")),
	}
}
