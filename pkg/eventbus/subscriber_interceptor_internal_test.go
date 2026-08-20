package eventbus

import (
	"context"
	"slices"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestChainInterceptorsReentrant is the regression test for the shared-index
// chain bug: `next` mutated a closure-shared position, so an interceptor
// calling next twice (a retry interceptor) skipped the interceptors between
// itself and the tail on the second pass. Each nesting level must own an
// immutable index (mirroring the publisher chain), making every next call
// re-run the SAME downstream chain.
func TestChainInterceptorsReentrant(t *testing.T) {
	var calls []string
	record := func(name string) SubscriberInterceptor {
		return func(ctx context.Context, md *event.Metadata, e any, next Handler) error {
			calls = append(calls, name)
			return next(ctx, e)
		}
	}
	retry := func(ctx context.Context, md *event.Metadata, e any, next Handler) error {
		calls = append(calls, "A")
		if err := next(ctx, e); err != nil {
			return err
		}
		return next(ctx, e) // second pass — must traverse B and C again
	}
	handler := func(ctx context.Context, e any) error {
		calls = append(calls, "H")
		return nil
	}

	chain := chainInterceptors([]SubscriberInterceptor{retry, record("B"), record("C")})
	if err := chain(context.Background(), event.NewMetadata("t"), struct{}{}, handler); err != nil {
		t.Fatalf("chain: %v", err)
	}

	want := []string{"A", "B", "C", "H", "B", "C", "H"}
	if !slices.Equal(calls, want) {
		t.Fatalf("call sequence = %v, want %v (second pass must not skip interceptors)", calls, want)
	}
}

// TestChainInterceptorsSequentialOrder pins the ordinary single-pass order for
// chains of length 1..3.
func TestChainInterceptorsSequentialOrder(t *testing.T) {
	for n := 1; n <= 3; n++ {
		var calls []string
		ints := make([]SubscriberInterceptor, 0, n)
		for i := range n {
			name := string(rune('A' + i))
			ints = append(ints, func(ctx context.Context, md *event.Metadata, e any, next Handler) error {
				calls = append(calls, name)
				return next(ctx, e)
			})
		}
		handler := func(ctx context.Context, e any) error {
			calls = append(calls, "H")
			return nil
		}

		if err := chainInterceptors(ints)(context.Background(), event.NewMetadata("t"), struct{}{}, handler); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		want := make([]string, 0, n+1)
		for i := range n {
			want = append(want, string(rune('A'+i)))
		}
		want = append(want, "H")
		if !slices.Equal(calls, want) {
			t.Fatalf("n=%d: call sequence = %v, want %v", n, calls, want)
		}
	}
}
