package db

import (
	"errors"
	"testing"
)

// mustIntent / mustNode / mustLink are terse builders for the delete-cascade tests.
func mustIntent(t *testing.T, es *ExplorationStore, summary string) int64 {
	t.Helper()
	id, err := es.AddIntent(map[string]any{"summary": summary}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustNode(t *testing.T, es *ExplorationStore, kind, summary string) int64 {
	t.Helper()
	id, err := es.AddNode(kind, map[string]any{"summary": summary}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustLink(t *testing.T, es *ExplorationStore, from int64, rel string, to int64) {
	t.Helper()
	if err := es.Link(from, rel, to); err != nil {
		t.Fatal(err)
	}
}

func gone(t *testing.T, es *ExplorationStore, id int64) bool {
	t.Helper()
	n, err := es.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	return n == nil
}

// TestSoftDeleteIntent 假删除置 deleted + delete_reason,保留节点。
func TestSoftDeleteIntent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("soft delete", "假删除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	intent := mustIntent(t, es, "待删意图")
	if err := es.SetIntentState(intent, "paused"); err != nil {
		t.Fatal(err)
	}
	summary, err := es.SoftDeleteIntent(intent, "方向判断错误")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "待删意图" {
		t.Fatalf("summary=%q, want 待删意图", summary)
	}
	n, err := es.GetNode(intent)
	if err != nil || n == nil {
		t.Fatalf("intent removed by soft delete: n=%+v err=%v", n, err)
	}
	if n.State != StateIntentDeleted || n.DeleteReason != "方向判断错误" {
		t.Fatalf("state=%q delete_reason=%q, want deleted/方向判断错误", n.State, n.DeleteReason)
	}
	// 待领(open)意图也允许假删除。
	openIntent := mustIntent(t, es, "待领意图")
	if _, err := es.SoftDeleteIntent(openIntent, "方向不需要了"); err != nil {
		t.Fatalf("soft delete open intent: %v", err)
	}
	if n, err := es.GetNode(openIntent); err != nil || n == nil || n.State != StateIntentDeleted {
		t.Fatalf("open intent not soft-deleted: n=%+v err=%v", n, err)
	}

	// 已删除(deleted)等其它状态不能再假删除。
	if _, err := es.SoftDeleteIntent(intent, "再删"); err == nil {
		t.Fatal("soft-deleting an already-deleted intent unexpectedly succeeded")
	}
}

// TestHardDeleteCascadesExclusiveDescendants 真删除沿链路级联删除独占子孙到叶子。
func TestHardDeleteCascadesExclusiveDescendants(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard cascade", "级联删除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// intent1 --yields--> fact1 --derived_from--> intent2 --yields--> fact2(叶子)
	intent1 := mustIntent(t, es, "根意图")
	fact1 := mustNode(t, es, KindFact, "事实1")
	mustLink(t, es, intent1, RelYields, fact1)
	intent2 := mustIntent(t, es, "衍生意图")
	mustLink(t, es, fact1, RelDerivedFrom, intent2)
	fact2 := mustNode(t, es, KindFact, "事实2")
	mustLink(t, es, intent2, RelYields, fact2)

	cleanup, err := es.CancelIntent(intent1)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 2 || cleanup.Facts != 2 {
		t.Fatalf("cleanup=%+v, want 2 intents / 2 facts", cleanup)
	}
	for _, id := range []int64{intent1, fact1, intent2, fact2} {
		if !gone(t, es, id) {
			t.Fatalf("node %d survived cascade", id)
		}
	}
}

// TestHardDeletePreservesSharedAndGoal 真删除保留共享子孙(还有其它父)与目标。
func TestHardDeletePreservesSharedAndGoal(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard preserve", "保留共享/目标")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	goal, err := es.AddGoal(map[string]any{"text": "拿下后台"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	// intent1 独占 finding(proves goal),intent1 与 intentX 共享 fact1(fact1 衍生出 intent2)。
	intent1 := mustIntent(t, es, "待删意图")
	intentX := mustIntent(t, es, "旁路意图")
	finding := mustNode(t, es, KindFinding, "漏洞")
	mustLink(t, es, intent1, RelYields, finding)
	mustLink(t, es, finding, RelProves, goal)
	shared := mustNode(t, es, KindFact, "共享事实")
	mustLink(t, es, intent1, RelYields, shared)
	mustLink(t, es, intentX, RelYields, shared)
	intent2 := mustIntent(t, es, "由共享事实衍生")
	mustLink(t, es, shared, RelDerivedFrom, intent2)

	cleanup, err := es.CancelIntent(intent1)
	if err != nil {
		t.Fatal(err)
	}
	// 只删 intent1 与其独占的 finding;shared(有 intentX 父)及其下游 intent2、goal 全保留。
	if cleanup.Intents != 1 || cleanup.Findings != 1 || cleanup.Facts != 0 {
		t.Fatalf("cleanup=%+v, want 1 intent / 1 finding / 0 fact", cleanup)
	}
	if !gone(t, es, intent1) || !gone(t, es, finding) {
		t.Fatal("intent1/finding should be removed")
	}
	for _, id := range []int64{goal, intentX, shared, intent2} {
		if gone(t, es, id) {
			t.Fatalf("node %d was wrongly cascaded", id)
		}
	}
}

// A worker can claim an open intent between the API's state read and its delete.
// Both deletes must refuse a running intent under the row lock — otherwise the
// intent is gone while its worker keeps executing and writing — and the blocked
// settlement of a run must never resurrect an intent the user deleted.
func TestDeleteRefusesRunningIntentAndSettlementCannotResurrect(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("delete race", "删除竞态")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	running := mustIntent(t, es, "被并发领走的意图")
	if claimed, err := es.ClaimIntent(running, "work#1"); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, err := es.SoftDeleteIntent(running, "删掉"); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("soft delete of a running intent: err=%v, want ErrIntentStateConflict", err)
	}
	if _, err := es.CancelIntent(running); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("hard delete of a running intent: err=%v, want ErrIntentStateConflict", err)
	}
	if n, err := es.GetNode(running); err != nil || n == nil || n.State != "running" {
		t.Fatalf("refused delete changed the intent: %+v %v", n, err)
	}

	// The run settles blocked while still running: allowed.
	if ok, err := es.SettleIntentBlocked(running, IntentBlockedLLMQuota); err != nil || !ok {
		t.Fatalf("settle running → blocked: ok=%v err=%v", ok, err)
	}

	// The user deletes (after the work stopped: paused), then a late settlement
	// arrives. It must not turn 'deleted' back into 'blocked'.
	late := mustIntent(t, es, "删除后才结算的意图")
	if err := es.SetIntentState(late, "paused"); err != nil {
		t.Fatal(err)
	}
	if _, err := es.SoftDeleteIntent(late, "不要了"); err != nil {
		t.Fatal(err)
	}
	if ok, err := es.SettleIntentBlocked(late, IntentBlockedLLMQuota); err != nil || ok {
		t.Fatalf("late settlement on a deleted intent: ok=%v err=%v, want no-op", ok, err)
	}
	if n, err := es.GetNode(late); err != nil || n == nil || n.State != StateIntentDeleted {
		t.Fatalf("deleted intent was resurrected: %+v %v", n, err)
	}
	if reopened, err := es.ReopenIntentsByBlockedReason(IntentBlockedLLMQuota); err != nil {
		t.Fatal(err)
	} else if n, _ := es.GetNode(late); n == nil || n.State != StateIntentDeleted || reopened != 1 {
		t.Fatalf("reopen touched the deleted intent or missed the blocked one: reopened=%d state=%+v", reopened, n)
	}
}

// A goalless task finishes when no intent can still run. One the user paused (to
// message it) is held, not finished: HasPendingIntent must count it, while
// HasActiveIntent — used for the first-round kick and stall prompts — must not.
func TestHasPendingIntentCountsUserPausedWork(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("pending intent", "暂停的意图不算跑完")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	intent := mustIntent(t, es, "用户暂停后要发消息的意图")
	if err := es.SetIntentState(intent, "paused"); err != nil {
		t.Fatal(err)
	}
	if active, err := es.HasActiveIntent(); err != nil || active {
		t.Fatalf("HasActiveIntent=%v err=%v, want false for a paused-only frontier", active, err)
	}
	if pending, err := es.HasPendingIntent(); err != nil || !pending {
		t.Fatalf("HasPendingIntent=%v err=%v, want true while an intent is paused", pending, err)
	}
	if err := es.SetIntentState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	if pending, err := es.HasPendingIntent(); err != nil || pending {
		t.Fatalf("HasPendingIntent=%v err=%v, want false once nothing can run", pending, err)
	}
}
