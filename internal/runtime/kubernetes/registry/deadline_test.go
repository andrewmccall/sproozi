package registry

import (
	"context"
	"testing"
	"time"
)

// A registration without a deadline is created, activated and stopped long
// after it was made, never expiring (ADR 0056).
func TestRegistrationWithoutDeadlineNeverExpires(t *testing.T) {
	s, _ := registry(t, nil)
	ctx := context.Background()
	reg := registration()
	reg.OriginalDeadline = time.Time{}
	r, err := s.Create(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindRun(ctx, r.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prepared(ctx, r.Reference, "pod-uid"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return testNow.Add(365 * 24 * time.Hour) }
	if r, err = s.Activate(ctx, r.Reference); err != nil || r.Phase != Active || !r.OriginalDeadline.IsZero() {
		t.Fatalf("activate without deadline: %+v %v", r, err)
	}
	if r, err = s.Stop(ctx, r.Reference); err != nil || r.Phase != "stopping" {
		t.Fatalf("stop without deadline: %+v %v", r, err)
	}
}
