package server

import (
	"context"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// A run started to deliver a human message that finds the task already paused
// has not delivered anything. It must hand the intent back as paused (so the
// message can be resent), not open — a pool worker would then re-run the intent
// without the message. A plain pool run still returns to the frontier.
func TestUndeliveredWorkerMessageParksIntentInsteadOfReopening(t *testing.T) {
	m, e, task := newGoallessFixture(t)
	_ = m
	e.Pause(task.ID, agent.AbortPausedByUser)

	run := func(message string) string {
		t.Helper()
		id, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "target"}, 1, "running", "planner", nil)
		if err != nil {
			t.Fatal(err)
		}
		node, err := task.Store.GetNode(id)
		if err != nil || node == nil {
			t.Fatalf("get intent: %v", err)
		}
		// The worker is never reached: the paused task cancels the run at its start.
		if !e.runIntent(context.Background(), task, "chat", new(agent.Worker), node, "req-1", message) {
			t.Fatal("runIntent reported nothing to settle")
		}
		after, err := task.Store.GetNode(id)
		if err != nil || after == nil {
			t.Fatalf("reload intent: %v", err)
		}
		return after.State
	}

	if state := run("please also check the admin panel"); state != "paused" {
		t.Fatalf("undelivered message left the intent %q, want paused", state)
	}
	if state := run(""); state != "open" {
		t.Fatalf("a pool run cancelled by pause left the intent %q, want open", state)
	}
}
