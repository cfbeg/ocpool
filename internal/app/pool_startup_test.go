package app

import (
	"context"
	"errors"
	"testing"
)

func TestAllocatedWorkerStartupOutlivesQueueWaitButHonorsRequest(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		s := testStore(t)
		addTestAccount(t, s, "cold")
		p := newPool(s, t.TempDir(), "")
		queue, stopQueue := context.WithCancel(context.Background())
		request, stopRequest := context.WithCancel(context.Background())
		p.startFn = func(ctx context.Context, id string) (*Runtime, error) {
			stopQueue()
			if cancelRequest {
				stopRequest()
				return nil, ctx.Err()
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &Runtime{ID: id, cancel: func() {}, subs: map[string]*subscription{}}, nil
		}
		r, e := p.acquire(queue, "opencode/test-free", "", request)
		if cancelRequest {
			if !errors.Is(e, context.Canceled) {
				t.Fatalf("request cancellation lost: %v", e)
			}
		} else {
			if e != nil {
				t.Fatal(e)
			}
			p.release(r, true)
		}
		stopRequest()
		stopQueue()
		p.close()
	}
}
