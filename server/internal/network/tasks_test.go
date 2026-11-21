package network

import (
	"strings"
	"testing"
	"time"
)

func TestTaskStoreSingleFlight(t *testing.T) {
	s := NewTaskStore()

	first, ok := s.Start("host-1", TaskKindInstall)
	if !ok {
		t.Fatal("first Start must succeed")
	}
	if first.Status != TaskRunning {
		t.Fatalf("new task must be running, got %s", first.Status)
	}

	// Same host, different kind: still conflicts (install restarts
	// tailscaled while a join would race it).
	second, ok := s.Start("host-1", TaskKindJoin)
	if ok {
		t.Fatal("second Start on the same host must be rejected")
	}
	if second.ID != first.ID {
		t.Fatalf("conflict must return the running task, got %s want %s", second.ID, first.ID)
	}

	// A different host is unaffected.
	if _, ok := s.Start("host-2", TaskKindInstall); !ok {
		t.Fatal("Start on another host must succeed")
	}
}

func TestTaskStoreFinishReleasesGuard(t *testing.T) {
	s := NewTaskStore()

	task, ok := s.Start("host-1", TaskKindInstall)
	if !ok {
		t.Fatal("first Start must succeed")
	}
	s.Finish(task, TaskSuccess, "ok", "some output")

	got, ok := s.Get(task.ID)
	if !ok {
		t.Fatal("finished task must stay pollable")
	}
	if got.Status != TaskSuccess || got.Message != "ok" || got.Detail != "some output" {
		t.Fatalf("unexpected finished task: %+v", got)
	}
	if got.DurationMs < 0 {
		t.Fatalf("duration must be recorded, got %d", got.DurationMs)
	}
	if got.FinishedAt == nil {
		t.Fatal("FinishedAt must be set")
	}

	// Guard released: a new op on the same host may start.
	if _, ok := s.Start("host-1", TaskKindJoin); !ok {
		t.Fatal("Start after Finish must succeed")
	}
}

func TestTaskStoreFinishClipsDetail(t *testing.T) {
	s := NewTaskStore()
	task, _ := s.Start("host-1", TaskKindInstall)
	s.Finish(task, TaskFailed, "failed", strings.Repeat("x", clipDetailLen+500))
	got, _ := s.Get(task.ID)
	if len(got.Detail) > clipDetailLen+50 {
		t.Fatalf("detail must be clipped, got %d bytes", len(got.Detail))
	}
}

func TestTaskStoreSetJoinResult(t *testing.T) {
	s := NewTaskStore()
	task, _ := s.Start("host-1", TaskKindJoin)
	s.SetJoinResult(task, "100.64.0.9", "100.64.0.7", true)
	s.Finish(task, TaskSuccess, "ok", "")
	got, _ := s.Get(task.ID)
	if got.IP != "100.64.0.9" || got.PreviousIP != "100.64.0.7" || !got.IPChanged {
		t.Fatalf("join result not recorded: %+v", got)
	}
}

func TestTaskStoreGetExpiresFinishedTasks(t *testing.T) {
	s := NewTaskStore()
	task, _ := s.Start("host-1", TaskKindInstall)
	s.Finish(task, TaskSuccess, "ok", "")

	// Age the finished task past the TTL (same package: fields are mutable).
	aged := time.Now().Add(-taskResultTTL - time.Minute)
	task.FinishedAt = &aged

	if _, ok := s.Get(task.ID); ok {
		t.Fatal("finished task older than TTL must read as not found")
	}
}

func TestTaskStoreGetKeepsRunningTasks(t *testing.T) {
	s := NewTaskStore()
	task, _ := s.Start("host-1", TaskKindInstall)
	if _, ok := s.Get(task.ID); !ok {
		t.Fatal("running task must stay pollable")
	}
}
