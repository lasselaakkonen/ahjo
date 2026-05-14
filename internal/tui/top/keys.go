package top

import "charm.land/bubbles/v2/key"

type keymap struct {
	Left, Right, Up, Down key.Binding
	Submit, Cancel        key.Binding
	Help, Quit            key.Binding
	Refresh               key.Binding

	// Contextual: repos column.
	AddRepo, RemoveRepo key.Binding

	// Contextual: containers column.
	NewContainer, RemoveContainer key.Binding

	// Contextual: details pane (when a worktree is selected).
	ToggleExpose, ToggleMirror key.Binding

	// Contextual: when a container is selected (in the containers column
	// or in the details pane showing that container's info).
	CopyClaudeCmd, CopyShellCmd key.Binding
	OpenIDE                     key.Binding
}

func newKeymap() keymap {
	return keymap{
		Left:    key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←↓↑→/hjkl", "")),
		Right:   key.NewBinding(key.WithKeys("right", "l")),
		Up:      key.NewBinding(key.WithKeys("up", "k")),
		Down:    key.NewBinding(key.WithKeys("down", "j")),
		Submit:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "submit")),
		Cancel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Refresh: key.NewBinding(key.WithKeys("f5"), key.WithHelp("F5", "refresh")),

		AddRepo:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add repo")),
		RemoveRepo: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove repo")),

		NewContainer:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "create container")),
		RemoveContainer: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove container")),

		ToggleExpose: key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "toggle expose")),
		ToggleMirror: key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "toggle mirror")),

		CopyClaudeCmd: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "copy `ahjo claude` cmd")),
		CopyShellCmd:  key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "copy `ahjo shell` cmd")),
		OpenIDE:       key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "ide")),
	}
}
