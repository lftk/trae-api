package main

import (
	"context"
	"errors"
	"fmt"
)

func runAPIOnly(ctx context.Context, c config) error {
	return runCurrent(ctx, c)
}

func runCombined(ctx context.Context, c config) error {
	auth, err := newAuthService(c, c.AuthAddr)
	if err != nil {
		return err
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		component string
		err       error
	}
	done := make(chan result, 2)
	go func() {
		done <- result{
			component: "API",
			err:       runAPIOnly(serviceCtx, c),
		}
	}()
	go func() {
		done <- result{
			component: "Auth",
			err:       auth.Run(serviceCtx),
		}
	}()
	first := <-done
	cancel()
	second := <-done
	var failures []error
	for _, result := range []result{first, second} {
		if result.err != nil {
			failures = append(failures, fmt.Errorf("%s stopped: %w", result.component, result.err))
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	if ctx.Err() == nil {
		return errors.New("component stopped unexpectedly")
	}
	return nil
}
