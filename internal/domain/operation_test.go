package domain

import (
	"errors"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

func TestCanTransitionLegal(t *testing.T) {
	cases := [][2]Status{
		{StatusPending, StatusRunning},
		{StatusPending, StatusCancelled},
		{StatusRunning, StatusSucceeded},
		{StatusRunning, StatusFailed},
		{StatusRunning, StatusCancelled},
	}
	for _, c := range cases {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("expected %s -> %s to be legal", c[0], c[1])
		}
	}
}

func TestCanTransitionIllegal(t *testing.T) {
	cases := [][2]Status{
		{StatusPending, StatusSucceeded},
		{StatusPending, StatusFailed},
		{StatusSucceeded, StatusRunning},
		{StatusFailed, StatusRunning},
		{StatusCancelled, StatusRunning},
		{StatusRunning, StatusPending},
		{StatusSucceeded, StatusFailed},
	}
	for _, c := range cases {
		if CanTransition(c[0], c[1]) {
			t.Errorf("expected %s -> %s to be illegal", c[0], c[1])
		}
	}
}

func TestRetryOnlyFromFailedOrCancelled(t *testing.T) {
	if !CanRetry(StatusFailed) || !CanRetry(StatusCancelled) {
		t.Fatal("failed and cancelled must be retryable")
	}
	for _, s := range []Status{StatusPending, StatusRunning, StatusSucceeded, StatusRolledBack} {
		if CanRetry(s) {
			t.Errorf("%s must not be retryable", s)
		}
	}
}

func TestTerminalAndValid(t *testing.T) {
	if !StatusSucceeded.Terminal() || !StatusFailed.Terminal() || !StatusCancelled.Terminal() {
		t.Fatal("succeeded/failed/cancelled must be terminal")
	}
	if StatusPending.Terminal() || StatusRunning.Terminal() {
		t.Fatal("pending/running must not be terminal")
	}
	if Status("bogus").Valid() {
		t.Fatal("unknown status must be invalid")
	}
}

func TestCodedErrorCodeOf(t *testing.T) {
	err := NewError(v1.CodeLockBusy, "resource %s busy", "demo")
	if CodeOf(err) != v1.CodeLockBusy {
		t.Fatalf("want %s, got %s", v1.CodeLockBusy, CodeOf(err))
	}
	if MessageOf(err) != "resource demo busy" {
		t.Fatalf("unexpected message: %s", MessageOf(err))
	}
	if CodeOf(errors.New("plain")) != v1.CodeInternal {
		t.Fatal("uncoded errors must map to INTERNAL")
	}
}
