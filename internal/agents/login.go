package agents

// Ready: Claude Code is installed here and logged in for the agents (`claude auth status`; a Claude Code too old to
// say counts as logged in, as for a session start).
func (m *Manager) Ready() bool {
	if _, ok := m.Available(); !ok {
		return false
	}
	m.mu.Lock()
	m.init()
	m.mu.Unlock()
	return m.loginError(m.homeEnv(DriverJSON)) == nil
}
