package serve

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const tokenCheckInterval = time.Second

func (s *Server) guard(ctx context.Context, token claims) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	var expiry *time.Timer
	if token.ExpiresAt != nil {
		// Expiry must end the stream even while a ledger read waits.
		expiry = time.AfterFunc(time.Until(token.ExpiresAt.Time), func() { cancel(errors.New("the token expired")) })
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(tokenCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := s.tokens.refresh(); err != nil {
				cancel(fmt.Errorf("read the token ledger: %w", err))

				return
			}
			entry, known := s.tokens.lookup(token.ID)
			if !known || entry.Revoked {
				cancel(errors.New("the token is revoked or absent from the ledger"))

				return
			}
		}
	}()

	return ctx, func() {
		if expiry != nil {
			expiry.Stop()
		}
		cancel(nil)
		<-done
	}
}
