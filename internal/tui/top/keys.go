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
}

func newKeymap() keymap {
	return keymap{
		Left:    key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "focus left")),
		Right:   key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "focus right")),
		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Submit:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "submit")),
		Cancel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Refresh: key.NewBinding(key.WithKeys("f5"), key.WithHelp("F5", "refresh")),

		AddRepo:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add repo")),
		RemoveRepo: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove repo")),

		NewContainer:    key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new container")),
		RemoveContainer: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove container")),

		ToggleExpose: key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "toggle expose")),
		ToggleMirror: key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "toggle mirror")),
	}
}
