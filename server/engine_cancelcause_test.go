package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

func TestPauseCancelsWithNamedCause(t *testing.T) {
	e := NewEngine(nil)
	execCtx := e.execContextFor(context.Background(), "t1")
	workCtx, workCancel := context.WithCancelCause(execCtx)
	defer workCancel(nil)
	e.Pause("t1", agent.AbortPausedByUser)
	if code, _, _, ok := agent.AbortReason(workCtx); !ok || code != "paused_by_user" {
		t.Fatalf("code=%q ok=%v, want paused_by_user", code, ok)
	}
}

func TestKillWorkCancelsOnlyThatWorkWithNamedCause(t *testing.T) {
	e := NewEngine(nil)
	execCtx := e.execContextFor(context.Background(), "t1")
	workCtx, workCancel := context.WithCancelCause(execCtx)
	e.registerWork(42, workCancel)
	if err := e.KillWork(42); err != nil {
		t.Fatal(err)
	}
	if code, _, _, ok := agent.AbortReason(workCtx); !ok || code != "killed_by_planner" {
		t.Fatalf("code=%q ok=%v, want killed_by_planner", code, ok)
	}
	if execCtx.Err() != nil {
		t.Fatal("kill_work cancelled the entire task context")
	}
	_, complete := e.detachWork(42)
	complete(nil)
}

func TestControlWorkCarriesUserActionCause(t *testing.T) {
	for _, tc := range []struct {
		action string
		code   string
	}{
		{action: "pause", code: "work_paused_by_user"},
		{action: "cancel", code: "work_cancelled_by_user"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			e := NewEngine(nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			e.registerWork(42, cancel)
			done := make(chan error, 1)
			go func() { done <- e.ControlWork(context.Background(), 42, tc.action) }()
			<-ctx.Done()
			if code, _, _, _ := agent.AbortReason(ctx); code != tc.code {
				t.Fatalf("code=%q, want %q", code, tc.code)
			}
			action, complete := e.detachWork(42)
			if action != tc.action {
				t.Fatalf("action=%q, want %q", action, tc.action)
			}
			complete(nil)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControlWorkReturnsSettlementError(t *testing.T) {
	e := NewEngine(nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	e.registerWork(42, cancel)
	done := make(chan error, 1)
	go func() { done <- e.ControlWork(context.Background(), 42, "pause") }()
	<-ctx.Done()
	_, complete := e.detachWork(42)
	want := errors.New("pause persistence failed")
	complete(want)
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("ControlWork error=%v, want %v", err, want)
	}
}

func TestControlWorkWaitHonorsContextAndReleasesReservation(t *testing.T) {
	e := NewEngine(nil)
	_, cancel := context.WithCancelCause(context.Background())
	e.registerWork(42, cancel)
	waitCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := e.ControlWork(waitCtx, 42, "pause"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ControlWork error=%v, want deadline exceeded", err)
	}

	e.workMu.Lock()
	run := e.work[42]
	if run == nil || run.action != "" {
		t.Fatalf("timed-out control left reservation: %+v", run)
	}
	e.workMu.Unlock()
	_, complete := e.detachWork(42)
	complete(nil)
}

func TestControlWorkRejectsConcurrentController(t *testing.T) {
	e := NewEngine(nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	e.registerWork(42, cancel)
	first := make(chan error, 1)
	go func() { first <- e.ControlWork(context.Background(), 42, "pause") }()
	<-ctx.Done()
	if err := e.ControlWork(context.Background(), 42, "cancel"); !errors.Is(err, errWorkControlConflict) {
		t.Fatalf("second control error=%v, want conflict", err)
	}
	action, complete := e.detachWork(42)
	if action != "pause" {
		t.Fatalf("winning action=%q, want pause", action)
	}
	complete(nil)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestSettleDrainAndGoalMetCausesAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		cause *agent.AbortCause
		code  string
	}{
		{agent.AbortSettleDrainTimeout, "settle_drain_timeout"},
		{agent.AbortGoalMet, "goal_met"},
	} {
		e := NewEngine(nil)
		ctx := e.execContextFor(context.Background(), "t1")
		e.cancelExec("t1", tc.cause)
		if code, _, _, _ := agent.AbortReason(ctx); code != tc.code {
			t.Fatalf("code=%q, want %q", code, tc.code)
		}
	}
}

func TestExecContextWhilePausedCarriesRaceGuardCause(t *testing.T) {
	e := NewEngine(nil)
	e.Pause("t1", agent.AbortPausedByUser)
	ctx := e.execContextFor(context.Background(), "t1")
	if ctx.Err() == nil {
		t.Fatal("paused task received a live execution context")
	}
	if code, _, _, _ := agent.AbortReason(ctx); code != "paused_race_guard" {
		t.Fatalf("code=%q, want paused_race_guard", code)
	}
}

func TestResumeYieldsFreshContext(t *testing.T) {
	e := NewEngine(nil)
	old := e.execContextFor(context.Background(), "t1")
	e.Pause("t1", agent.AbortPausedByUser)
	if old.Err() == nil {
		t.Fatal("Pause did not cancel old context")
	}
	e.paused.Delete("t1")
	if fresh := e.execContextFor(context.Background(), "t1"); fresh.Err() != nil {
		t.Fatal("resume did not create a fresh execution context")
	}
}

func TestTaskPauseCauseSurvivesImmediateResume(t *testing.T) {
	e := NewEngine(nil)
	task := &Task{ID: "t1", notify: make(chan struct{}, 1)}
	old := e.execContextFor(context.Background(), task.ID)
	e.Pause(task.ID, agent.AbortPausedByUser)
	e.Resume(task)
	if e.IsPaused(task.ID) {
		t.Fatal("resume did not clear the task pause flag")
	}
	if old.Err() == nil || !taskExecutionPaused(context.Cause(old)) {
		t.Fatalf("old execution cause=%v was not retained as a task pause", context.Cause(old))
	}
	if taskExecutionPaused(agent.AbortGoalMet) {
		t.Fatal("goal completion was misclassified as a task pause")
	}
}

func TestDeleteCancellationReturnsRunningIntentToFrontier(t *testing.T) {
	if !taskExecutionPaused(agent.AbortTaskDeleted) {
		t.Fatal("delete cancellation must settle a running intent as open so AbortDelete can resume it")
	}
	if taskExecutionPaused(agent.AbortGoalMet) {
		t.Fatal("terminal task cancellation must not reopen completed work")
	}
}

// A shutdown cancels the process root context; workers run on contexts derived
// from it. Their in-flight intents must go back to the frontier (open), not be
// settled as blocked — the restart only resets rows still marked running.
func TestShutdownCancellationReturnsRunningIntentToFrontier(t *testing.T) {
	if !taskExecutionPaused(agent.AbortShutdown) {
		t.Fatal("shutdown cancellation must settle a running intent as open so the next process re-claims it")
	}
	root, cancel := context.WithCancelCause(context.Background())
	e := NewEngine(nil)
	child := e.execContextFor(root, "t1")
	cancel(agent.AbortShutdown)
	<-child.Done()
	if !taskExecutionPaused(context.Cause(child)) {
		t.Fatalf("derived execution context cause=%v was not recognized as shutdown", context.Cause(child))
	}
}

func TestResumeDoesNotClearDeleteBarrier(t *testing.T) {
	e := NewEngine(nil)
	task := &Task{ID: "t1", notify: make(chan struct{}, 1)}
	e.paused.Store(task.ID, true)
	e.deleting.Store(task.ID, true)
	e.Resume(task)
	if !e.IsPaused(task.ID) {
		t.Fatal("resume cleared the pause barrier owned by task deletion")
	}
}

func TestBootstrapAdmissionClearsQueuedPauseBarrier(t *testing.T) {
	e := NewEngine(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	task := &Task{ID: "t1", notify: make(chan struct{}, 1)}
	s := &Server{engine: e, ctx: ctx}
	e.Pause(task.ID, agent.AbortPausedByUser)

	s.startAdmittedTask(task, "bootstrap")
	if e.IsPaused(task.ID) {
		t.Fatal("bootstrap admission left the queued task behind the pause barrier")
	}
}

// Worker loops outlive a finished task on purpose (a rerun revives it and Run
// only nudges existing loops), so their idling must cost nothing. Resolving the
// task's agents reads the database; it used to happen before the terminal check,
// so every worker of every finished task queried once a second, forever.
func TestWorkerLoopIdlesTerminalTaskWithoutResolvingAgents(t *testing.T) {
	task := &Task{ID: "t-done", notify: make(chan struct{}, 1)}
	task.updateLifecycle(func(state *taskLifecycleState) { state.Status = "done" })
	e := NewEngine(&Manager{tasks: map[string]*Task{task.ID: task}})
	var resolves atomic.Int32
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		resolves.Add(1)
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	e.workerLoop(ctx, task, "w1") // returns once ctx ends
	if n := resolves.Load(); n != 0 {
		t.Fatalf("worker loop resolved agents %d times for a finished task; idling must not touch the database", n)
	}
}

// A steer that saw the work alive must never leave a message behind once the
// work is detached: the next re-run of the same intent would receive a stale
// course-correction meant for the old run. Stress the check-then-append window.
func TestSteerWorkNeverStrandsMessageAfterDetach(t *testing.T) {
	e := NewEngine(nil)
	const intentID = 42
	for i := 0; i < 5000; i++ {
		_, cancel := context.WithCancelCause(context.Background())
		e.registerWork(intentID, cancel)
		done := make(chan struct{})
		go func() {
			_ = e.SteerWork(intentID, "steer")
			close(done)
		}()
		e.detachWork(intentID)
		<-done
		e.steerMu.Lock()
		left := len(e.steerBox[intentID])
		delete(e.steerBox, intentID)
		e.steerMu.Unlock()
		if left != 0 {
			t.Fatalf("iteration %d: %d steering message(s) stranded after the work was detached", i, left)
		}
	}
}

// A worker that reaches the delete barrier must park, not exit: when the delete
// is aborted, AbortDelete clears the flags but nothing restarts loops (Run only
// nudges a task that is still started), so a returned worker was lost for good.
func TestWorkerParksDuringDeleteAndResumesAfterAbort(t *testing.T) {
	task := &Task{ID: "t-del", notify: make(chan struct{}, 1)}
	e := NewEngine(&Manager{tasks: map[string]*Task{task.ID: task}})
	var resolves atomic.Int32
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		resolves.Add(1)
		return nil, nil // no LLM: an idle worker resolves, sleeps, repeats
	})
	// Only the deleting flag, as a worker sees it in the window before BeginDelete
	// raises the pause barrier.
	e.deleting.Store(task.ID, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.workerLoop(ctx, task, "w1")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("worker exited on the delete barrier; an aborted delete would lose it")
	case <-time.After(1500 * time.Millisecond):
	}
	if n := resolves.Load(); n != 0 {
		t.Fatalf("parked worker resolved agents %d times", n)
	}
	e.AbortDelete(task.ID, false)
	deadline := time.Now().Add(5 * time.Second)
	for resolves.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker did not resume after the delete was aborted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel() // a completed delete ends the loop through its context
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit when its context was cancelled")
	}
}
