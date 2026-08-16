package services

import (
	"context"
	"io"
	"testing"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestClientGoneClassification pins the split between "the client left" and
// "we broke". Getting this wrong in either direction is expensive and neither
// direction is loud: mislabel a disconnect as an error and the service's error
// rate becomes a traffic counter (the state this test was written to end —
// 97% of the seeder's daily error records were closed browser tabs); mislabel
// a real fault as a disconnect and it disappears from the logs entirely.
func TestClientGoneClassification(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a disconnect", nil, false},
		{"bare context.Canceled", context.Canceled, true},
		{"wrapped context.Canceled", errors.Wrap(context.Canceled, "failed to send stat"), true},
		{"io.EOF", io.EOF, true},
		{"wrapped io.EOF", errors.Wrap(io.EOF, "stream read"), true},
		{"grpc Canceled status", status.Error(codes.Canceled, "context canceled"), true},
		{"grpc Unavailable status", status.Error(codes.Unavailable, "transport closing"), true},

		// The negative half. These must keep reaching the logs at Error —
		// they are the failures the seeder actually needs to report.
		{"deadline exceeded is ours, not theirs", context.DeadlineExceeded, false},
		{"grpc NotFound", status.Error(codes.NotFound, "unable to find file for path=x"), false},
		{"grpc Internal", status.Error(codes.Internal, "boom"), false},
		{"grpc DeadlineExceeded", status.Error(codes.DeadlineExceeded, "too slow"), false},
		{"plain error", errors.New("no info-hash provided"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientGone(tt.err); got != tt.want {
				t.Fatalf("clientGone(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestClientGoneCoversContextDoneBranch is the regression guard for the
// specific defect: StatStream's `case <-stream.Context().Done()` arm read
// Context.Err() and logged it at Error whenever it was non-nil — which,
// inside that branch, is always. The `else` arm that would have logged the
// ordinary completion was unreachable code.
//
// Asserting the Go contract directly, because the whole bug was a wrong
// assumption about it: once Done() is closed, Err() never returns nil, so a
// cancelled stream MUST be classified as a disconnect or the Error arm fires
// on every one of them.
func TestClientGoneCoversContextDoneBranch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	<-ctx.Done()

	err := ctx.Err()
	if err == nil {
		t.Fatal("ctx.Err() was nil after Done() closed; the unreachable else-arm would be live")
	}
	if !clientGone(err) {
		t.Fatalf("clientGone(%v) = false: a cancelled stream would log at Error on every client disconnect", err)
	}
}
