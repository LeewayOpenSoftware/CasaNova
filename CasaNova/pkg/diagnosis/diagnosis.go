// Package diagnosis provides a pluggable health-check engine for CasaNova.
//
// Inspired by YunoHost's self-diagnosis system: instead of making users
// guess why something isn't working, the system checks everything itself
// and reports findings in plain language.
//
// Each check is a Diagnoser — a function that runs a specific test and
// returns structured results. The engine runs all checks and aggregates
// the output for the UI.
package diagnosis

import (
	"context"
	"sync"
	"time"
)

// Severity represents the severity level of a diagnostic finding.
type Severity string

const (
	SeverityInfo    Severity = "info"    // Informational
	SeverityOK      Severity = "ok"      // Everything is fine
	SeverityWarning Severity = "warning" // Something to watch
	SeverityError   Severity = "error"   // Active problem
)

// Category groups related diagnostic checks.
type Category string

const (
	CategorySystem    Category = "system"    // Kernel, OS, hardware
	CategoryServices  Category = "services"  // CasaNova services
	CategoryStorage   Category = "storage"   // Disks, filesystems
	CategoryNetwork   Category = "network"   // DNS, ports, connectivity
	CategoryContainer Category = "container" // Docker, apps
	CategorySecurity  Category = "security"  // Firewall, updates
)

// Result is a single diagnostic finding.
type Result struct {
	Category Category  `json:"category"`
	Check    string    `json:"check"`   // Short identifier, e.g. "docker-daemon"
	Title    string    `json:"title"`   // Human-readable title
	Summary  string    `json:"summary"` // One-line summary
	Details  string    `json:"details"` // Extended explanation (optional)
	Severity Severity  `json:"severity"`
	Fix      string    `json:"fix,omitempty"` // Suggested action (optional)
	Checked  time.Time `json:"checked_at"`
}

// Diagnoser is a function that performs a single diagnostic check.
// It should be self-contained, fast (< 2 seconds), and return quickly.
// Diagnosers MUST be safe to run concurrently.
type Diagnoser func(ctx context.Context) Result

// Engine runs diagnosers and aggregates results.
type Engine struct {
	mu         sync.RWMutex
	diagnosers []Diagnoser
	lastRun    []Result
	lastRunAt  time.Time
	cacheTTL   time.Duration
}

// NewEngine creates a diagnosis engine with default cache TTL of 5 minutes.
func NewEngine() *Engine {
	return &Engine{
		diagnosers: []Diagnoser{},
		cacheTTL:   5 * time.Minute,
	}
}

// Register adds a diagnoser to the engine. Must be called before Run.
func (e *Engine) Register(d Diagnoser) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.diagnosers = append(e.diagnosers, d)
}

// Run executes all registered diagnosers in parallel and returns results.
// If results are cached within the TTL, returns cached results instead.
// Pass force=true to bypass cache.
func (e *Engine) Run(ctx context.Context, force bool) []Result {
	e.mu.RLock()
	if !force && len(e.lastRun) > 0 && time.Since(e.lastRunAt) < e.cacheTTL {
		cached := make([]Result, len(e.lastRun))
		copy(cached, e.lastRun)
		e.mu.RUnlock()
		return cached
	}
	diagnosers := make([]Diagnoser, len(e.diagnosers))
	copy(diagnosers, e.diagnosers)
	e.mu.RUnlock()

	// Run all diagnosers in parallel with a timeout
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	results := make([]Result, len(diagnosers))
	var wg sync.WaitGroup
	for i, d := range diagnosers {
		wg.Add(1)
		go func(idx int, diag Diagnoser) {
			defer wg.Done()
			defer func() {
				// Never let a panicking diagnoser take down the engine
				if r := recover(); r != nil {
					results[idx] = Result{
						Check:    "unknown",
						Title:    "Diagnoser panicked",
						Summary:  "A diagnostic check crashed — this is a bug in the check itself",
						Severity: SeverityError,
						Checked:  time.Now(),
					}
				}
			}()
			results[idx] = diag(runCtx)
			if results[idx].Checked.IsZero() {
				results[idx].Checked = time.Now()
			}
		}(i, d)
	}
	wg.Wait()

	// Do NOT cache a run that was cut short by the CALLER's context (client
	// disconnected, request canceled). Every check would have failed with a
	// context-canceled error and cached those bogus SeverityError results —
	// poisoning the 5-minute window so healthy boxes report "Docker not
	// reachable" to everyone until the TTL expires. Only the run's OWN 30s
	// timeout (runCtx alone firing) is a legitimate result worth caching.
	if ctx.Err() != nil {
		return results
	}

	e.mu.Lock()
	e.lastRun = results
	e.lastRunAt = time.Now()
	e.mu.Unlock()

	return results
}

// Summary returns counts of results by severity.
func Summary(results []Result) map[Severity]int {
	counts := map[Severity]int{
		SeverityOK:      0,
		SeverityInfo:    0,
		SeverityWarning: 0,
		SeverityError:   0,
	}
	for _, r := range results {
		counts[r.Severity]++
	}
	return counts
}

// ByCategory groups results by category.
func ByCategory(results []Result) map[Category][]Result {
	groups := make(map[Category][]Result)
	for _, r := range results {
		groups[r.Category] = append(groups[r.Category], r)
	}
	return groups
}

// HasErrors returns true if any result has Error severity.
func HasErrors(results []Result) bool {
	for _, r := range results {
		if r.Severity == SeverityError {
			return true
		}
	}
	return false
}
