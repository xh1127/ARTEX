package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// newGoallessFixture builds an engine around a fresh task with no goal node —
// the state a restart during launchTask's goal decomposition leaves behind.
// The planner pair is never asked to plan: every round here ends in the
// goalless branch before planner.Plan.
func newGoallessFixture(t *testing.T) (*Manager, *Engine, *Task) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { m.Close() }) // registered first, runs last
	task, err := m.CreateTask("never decomposed", "restart hit goal decomposition", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	e := NewEngine(m)
	e.debounce = 10 * time.Millisecond
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return new(agent.Planner), new(agent.Worker)
	})
	return m, e, task
}

func runPlannerLoop(t *testing.T, ctx context.Context, e *Engine, task *Task) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		e.plannerLoop(ctx, task)
		close(done)
	}()
	task.Notify()
	return done
}

// A task with no goal node at all was never decomposed; it is not "all goals
// met". The goalless round used to finish it ("目标已全部达成") without it ever
// running. It must decompose instead.
func TestGoallessRoundDecomposesANeverDecomposedTask(t *testing.T) {
	m, e, task := newGoallessFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	e.SetGoalBootstrap(func(_ context.Context, tk *Task) {
		calls.Add(1)
		if _, err := tk.Store.AddNode(db.KindGoal, map[string]any{"text": "decomposed goal"}, 0, "open", "system", nil); err != nil {
			t.Error(err)
		}
		cancel() // stop here: the next round would plan with the stub planner
	})
	done := runPlannerLoop(t, ctx, e, task)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("planner loop did not reach goal decomposition")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("goal decomposition ran %d times, want 1", n)
	}
	if status := m.TaskStatus(task.ID); isTerminalStatus(status) {
		t.Fatalf("never-decomposed task was settled as %q", status)
	}
}

// Decomposition runs once per process. A task that still has no goal afterwards
// (an empty task goal) is genuinely goalless and finishes once its frontier is
// drained — it must not re-run the LLM decomposition every round.
func TestGoallessRoundDecomposesOnceThenFinishesGenuinelyGoalless(t *testing.T) {
	m, e, task := newGoallessFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	e.SetGoalBootstrap(func(context.Context, *Task) { calls.Add(1) }) // yields no goal
	done := runPlannerLoop(t, ctx, e, task)
	deadline := time.Now().Add(10 * time.Second)
	for !isTerminalStatus(m.TaskStatus(task.ID)) {
		if time.Now().After(deadline) {
			t.Fatalf("genuinely goalless task never finished (status %q)", m.TaskStatus(task.ID))
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if n := calls.Load(); n != 1 {
		t.Fatalf("goal decomposition ran %d times, want exactly 1", n)
	}
}

// The resume path (startTaskEngine) and a planner round can both reach a
// never-decomposed task; the claim makes the decomposition happen once.
func TestGoalBootstrapClaimIsExclusive(t *testing.T) {
	e := NewEngine(nil)
	if !e.beginGoalBootstrap("t1") {
		t.Fatal("first claim refused")
	}
	if e.beginGoalBootstrap("t1") || !e.goalBootstrapRunning("t1") {
		t.Fatal("second claim granted while the first runs")
	}
	e.abortGoalBootstrap("t1") // cut short by pause: may be claimed again
	if !e.beginGoalBootstrap("t1") {
		t.Fatal("aborted decomposition cannot be retried")
	}
	e.endGoalBootstrap("t1")
	if e.beginGoalBootstrap("t1") || e.goalBootstrapRunning("t1") {
		t.Fatal("finished decomposition claimed again or still reported running")
	}
}
