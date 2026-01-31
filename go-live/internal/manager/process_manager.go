package manager

import (
	"context"
	"sync"
	"time"
)

type GeneratorProcess struct {
	ID        string
	Input     string
	Output    string
	StartTime time.Time
	Cancel    context.CancelFunc `json:"-"`
}

type ProcessManager struct {
	processes map[string]*GeneratorProcess
	mu        sync.RWMutex
}

func NewProcessManager() *ProcessManager {
	return &ProcessManager{processes: make(map[string]*GeneratorProcess)}
}

func (pm *ProcessManager) Spawn(id, input, output string, cancel context.CancelFunc) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.processes[id] = &GeneratorProcess{
		ID: id, Input: input, Output: output, StartTime: time.Now(), Cancel: cancel,
	}
}

func (pm *ProcessManager) Stop(id string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if proc, ok := pm.processes[id]; ok {
		proc.Cancel()
		delete(pm.processes, id)
	}
}

func (pm *ProcessManager) IsRunning(id string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	_, exists := pm.processes[id]
	return exists
}

func (pm *ProcessManager) Status() []*GeneratorProcess {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	procs := make([]*GeneratorProcess, 0, len(pm.processes))
	for _, p := range pm.processes {
		procs = append(procs, p)
	}
	return procs
}
