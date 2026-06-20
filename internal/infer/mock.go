package infer

import "sync"

// MockModel is a deterministic inference model for tests. It echoes a fixed
// prefix plus the prompt and records call counts so tests can assert behavior.
type MockModel struct {
	Name string

	mu    sync.Mutex
	calls int
}

// NewMockModel returns a mock inference model with the given name.
func NewMockModel(name string) *MockModel {
	if name == "" {
		name = "mock"
	}
	return &MockModel{Name: name}
}

// ModelName returns the model identifier.
func (m *MockModel) ModelName() string { return m.Name }

// Complete returns a deterministic completion derived from the prompt.
func (m *MockModel) Complete(prompt string) (string, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return "context: " + prompt, nil
}

// Calls returns the number of Complete invocations.
func (m *MockModel) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
