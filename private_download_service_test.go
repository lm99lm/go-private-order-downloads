package main

import (
	"context"
	"testing"
	"time"
)

type fakeStore struct {
	found        bool
	link         string
	headCalls    int
	presignCalls int
}

func (f *fakeStore) headObject(context.Context, string, string) (bool, error) {
	f.headCalls++
	return f.found, nil
}
func (f *fakeStore) presignDownload(context.Context, string, string, string, time.Duration) (string, error) {
	f.presignCalls++
	return f.link, nil
}

func TestFulfillmentDecision(t *testing.T) {
	tests := []struct {
		name        string
		found       bool
		wantStatus  orderStatus
		wantError   bool
		wantPresign int
	}{
		{name: "paid order with stored file becomes ready", found: true, wantStatus: statusReady, wantPresign: 1},
		{name: "paid order waits while file is absent", found: false, wantStatus: statusPaid, wantError: true, wantPresign: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{found: tt.found, link: "https://download.example/signed"}
			service := newOrderService("orders", store)
			service.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
			if _, err := service.checkout("ord-42", "cus-7"); err != nil {
				t.Fatal(err)
			}
			_, link, err := service.fulfill(context.Background(), "ord-42", "receipts/ord-42.pdf", "req-42")
			if (err != nil) != tt.wantError {
				t.Fatalf("fulfill error = %v, wantError %v", err, tt.wantError)
			}
			got, _ := service.getOrder("ord-42")
			if got.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tt.wantStatus)
			}
			if store.presignCalls != tt.wantPresign {
				t.Fatalf("presign calls = %d, want %d", store.presignCalls, tt.wantPresign)
			}
			if !tt.wantError && link == "" {
				t.Fatal("expected a signed download URL")
			}
		})
	}
}
