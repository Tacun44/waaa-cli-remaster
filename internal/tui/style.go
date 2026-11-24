package tui

import "github.com/charmbracelet/lipgloss"

var (
	sidebarWidth = 28

	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	chatListHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("205"))

	selectedChatStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).
				Background(lipgloss.Color("205")).
				Bold(true)

	chatItemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	unreadStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")).
			Bold(true)

	myMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("86"))

	theirMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	senderNameStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))

	timestampStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#a6adc8"))

	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	inputBoxFocusedStyle = inputBoxStyle.
				BorderForeground(lipgloss.Color("205"))

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#a6adc8"))

	onlineStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("82"))

	botOnStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("82"))

	lastSeenStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#a6adc8"))

	quoteStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#a6adc8")).
			Italic(true)

	replySelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("205"))

	jumpHighlightStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("214"))

	dateSeparatorStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				Bold(true)

	replyBannerStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("205")).
				Background(lipgloss.Color("236")).
				Padding(0, 1)

	transcriptLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("141"))

	documentBubbleStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("141")).
				Padding(0, 1)

	readReceiptStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("39"))

	playedAudioStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("39"))

	possibleBlockStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("214"))

	deleteModalStyle = lipgloss.NewStyle().
				Border(lipgloss.DoubleBorder()).
				BorderForeground(lipgloss.Color("196")).
				Foreground(lipgloss.Color("252")).
				Background(lipgloss.Color("236")).
				Padding(1, 3).
				Align(lipgloss.Center)

	contextMenuStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("205")).
				Foreground(lipgloss.Color("252")).
				Background(lipgloss.Color("236")).
				Padding(0, 2)

	contextMenuItemStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))

	contextMenuItemSelectedStyle = lipgloss.NewStyle().
					Bold(true).
					Foreground(lipgloss.Color("0")).
					Background(lipgloss.Color("205"))
)
