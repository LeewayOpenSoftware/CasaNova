package service

import "testing"

// The notify loop iterates a snapshot of the queue and removes entries as
// they finish, which is not necessarily front-to-back: a small copy queued
// second finishes while a large one queued first is still running. Removing
// by position dropped the running operation from progress reporting and
// leaked its FileQueue entry; draining past the end panicked.
func TestRemoveOpDropsByIdentityNotPosition(t *testing.T) {
	OpMu.Lock()
	OpStrArr = []string{"big", "small", "medium"}
	OpMu.Unlock()

	RemoveOp("small")

	OpMu.Lock()
	got := append([]string{}, OpStrArr...)
	OpMu.Unlock()

	want := []string{"big", "medium"}
	if len(got) != len(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue = %v, want %v", got, want)
		}
	}
}

func TestRemoveOpOnEmptyQueueDoesNotPanic(t *testing.T) {
	OpMu.Lock()
	OpStrArr = nil
	OpMu.Unlock()

	RemoveOp("gone") // used to panic on OpStrArr[1:]

	RemoveOp("")
}

func TestRemoveOpUnknownIDLeavesQueueIntact(t *testing.T) {
	OpMu.Lock()
	OpStrArr = []string{"a", "b"}
	OpMu.Unlock()

	RemoveOp("c")

	OpMu.Lock()
	defer OpMu.Unlock()
	if len(OpStrArr) != 2 || OpStrArr[0] != "a" || OpStrArr[1] != "b" {
		t.Fatalf("queue = %v, want [a b]", OpStrArr)
	}
}
