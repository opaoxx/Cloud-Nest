package main

import (
	"context"
	"errors"
	"testing"
)

func TestTransferLifecycleStopsAdmissionAndWaitsForCleanup(t *testing.T) {
	lifecycle := &transferLifecycle{}
	upload, finishUpload, err := lifecycle.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	download, finishDownload, err := lifecycle.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drained, started := lifecycle.stop()
	if !started || !errors.Is(context.Cause(upload), errTransferStopping) ||
		!errors.Is(context.Cause(download), errTransferStopping) {
		t.Fatal("exit did not notify both transfers")
	}
	if _, _, err := lifecycle.begin(t.Context()); !errors.Is(err, errTransferStopping) {
		t.Fatalf("new work admitted during shutdown: %v", err)
	}
	if transferCheckpointContext(download).Err() != nil {
		t.Fatal("download cannot sync its confirmed checkpoint during exit")
	}
	finishDownload()
	select {
	case <-drained:
		t.Fatal("exit finished while upload still held its resources")
	default:
	}
	finishUpload()
	select {
	case <-drained:
	default:
		t.Fatal("exit did not finish after transfers released their resources")
	}
}

func TestTransferLifecycleWakeDoesNotCancelTheNextAttempt(t *testing.T) {
	lifecycle := &transferLifecycle{}
	_, err := runDesktopTransfer(t.Context(), lifecycle, func(ctx context.Context) (int, error) {
		lifecycle.pauseAfterWake()
		if transferCheckpointContext(ctx).Err() != nil {
			t.Fatal("interrupted operation cannot save its checkpoint")
		}
		return 0, ctx.Err()
	})
	if !errors.Is(err, errTransferWake) {
		t.Fatalf("wake reason was lost: %v", err)
	}
	ctx, finish, err := lifecycle.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if ctx.Err() != nil {
		t.Fatal("manual retry was canceled by past wake event")
	}
}
