package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/IUCLabs/artifactapi/pkg/client"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type view int

const (
	viewDashboard view = iota
	viewRemotes
	viewRemoteDetail
	viewObjects
	viewVirtuals
)

type model struct {
	client  *client.Client
	view    view
	width   int
	height  int
	err     error
	loading bool

	stats    *models.OverviewStats
	remotes  []models.Remote
	virtuals []models.Virtual
	objects  []models.Artifact

	selectedRemote string
	cursor         int
	page           int
}

func New(endpoint string) *model {
	return &model{
		client:  client.New(endpoint),
		view:    viewDashboard,
		loading: true,
		page:    1,
	}
}

func (m *model) Run() error {
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m *model) Init() tea.Cmd {
	return m.loadDashboard()
}

type dashboardLoaded struct {
	stats    *models.OverviewStats
	remotes  []models.Remote
	virtuals []models.Virtual
}

type remotesLoaded struct{ remotes []models.Remote }
type virtualsLoaded struct{ virtuals []models.Virtual }
type objectsLoaded struct{ objects []models.Artifact }
type errMsg struct{ err error }

func (m *model) loadDashboard() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		stats, err := m.client.Stats(ctx)
		if err != nil {
			return errMsg{err}
		}
		remotes, _ := m.client.ListRemotes(ctx)
		virtuals, _ := m.client.ListVirtuals(ctx)
		return dashboardLoaded{stats: stats, remotes: remotes, virtuals: virtuals}
	}
}

func (m *model) loadRemotes() tea.Cmd {
	return func() tea.Msg {
		remotes, err := m.client.ListRemotes(context.Background())
		if err != nil {
			return errMsg{err}
		}
		return remotesLoaded{remotes}
	}
}

func (m *model) loadVirtuals() tea.Cmd {
	return func() tea.Msg {
		virtuals, err := m.client.ListVirtuals(context.Background())
		if err != nil {
			return errMsg{err}
		}
		return virtualsLoaded{virtuals}
	}
}

func (m *model) loadObjects() tea.Cmd {
	return func() tea.Msg {
		objects, err := m.client.ListObjects(context.Background(), m.selectedRemote, m.page, 30)
		if err != nil {
			return errMsg{err}
		}
		return objectsLoaded{objects}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case dashboardLoaded:
		m.loading = false
		m.stats = msg.stats
		m.remotes = msg.remotes
		m.virtuals = msg.virtuals
		return m, nil

	case remotesLoaded:
		m.loading = false
		m.remotes = msg.remotes
		m.cursor = 0
		return m, nil

	case virtualsLoaded:
		m.loading = false
		m.virtuals = msg.virtuals
		m.cursor = 0
		return m, nil

	case objectsLoaded:
		m.loading = false
		m.objects = msg.objects
		m.cursor = 0
		return m, nil

	case errMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	}

	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		if m.view == viewDashboard {
			return m, tea.Quit
		}
		m.view = viewDashboard
		m.cursor = 0
		m.loading = true
		return m, m.loadDashboard()

	case "esc":
		switch m.view {
		case viewRemoteDetail, viewObjects:
			m.view = viewRemotes
			m.cursor = 0
			m.loading = true
			return m, m.loadRemotes()
		case viewRemotes, viewVirtuals:
			m.view = viewDashboard
			m.cursor = 0
			m.loading = true
			return m, m.loadDashboard()
		default:
			return m, tea.Quit
		}

	case "1":
		m.view = viewDashboard
		m.loading = true
		return m, m.loadDashboard()

	case "2":
		m.view = viewRemotes
		m.loading = true
		return m, m.loadRemotes()

	case "3":
		m.view = viewVirtuals
		m.loading = true
		return m, m.loadVirtuals()

	case "j", "down":
		m.cursor++
		m.clampCursor()
		return m, nil

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "enter":
		return m.handleEnter()

	case "r":
		m.loading = true
		switch m.view {
		case viewDashboard:
			return m, m.loadDashboard()
		case viewRemotes:
			return m, m.loadRemotes()
		case viewVirtuals:
			return m, m.loadVirtuals()
		case viewObjects:
			return m, m.loadObjects()
		}
	}

	return m, nil
}

func (m *model) handleEnter() (tea.Model, tea.Cmd) {
	switch m.view {
	case viewRemotes:
		if m.cursor < len(m.remotes) {
			m.selectedRemote = m.remotes[m.cursor].Name
			m.view = viewRemoteDetail
			return m, nil
		}
	case viewRemoteDetail:
		m.view = viewObjects
		m.page = 1
		m.loading = true
		return m, m.loadObjects()
	}
	return m, nil
}

func (m *model) clampCursor() {
	max := 0
	switch m.view {
	case viewRemotes:
		max = len(m.remotes) - 1
	case viewVirtuals:
		max = len(m.virtuals) - 1
	case viewObjects:
		max = len(m.objects) - 1
	}
	if m.cursor > max {
		m.cursor = max
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) View() string {
	if m.loading {
		return m.chrome("Loading...")
	}
	if m.err != nil {
		return m.chrome(errStyle.Render(fmt.Sprintf("Error: %v", m.err)))
	}

	var body string
	switch m.view {
	case viewDashboard:
		body = m.viewDashboard()
	case viewRemotes:
		body = m.viewRemotesList()
	case viewRemoteDetail:
		body = m.viewRemoteDetail()
	case viewObjects:
		body = m.viewObjectsList()
	case viewVirtuals:
		body = m.viewVirtualsList()
	}

	return m.chrome(body)
}

func (m *model) chrome(body string) string {
	nav := navStyle.Render(
		"[1] Dashboard  [2] Remotes  [3] Virtuals  │  [r] Refresh  [q] Quit",
	)
	return lipgloss.JoinVertical(lipgloss.Left, body, "", nav)
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	navStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	selStyle   = lipgloss.NewStyle().Background(lipgloss.Color("4")).Foreground(lipgloss.Color("15"))
)
