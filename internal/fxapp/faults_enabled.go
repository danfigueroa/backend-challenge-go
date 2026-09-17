//go:build faultinject

package fxapp

import (
	"context"

	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/sqsconsumer"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/faultinject"
	"github.com/danfigueroa/backend-challenge-go/internal/worker/outboxpub"
)

var faultInjection = fx.Supply(
	sqsconsumer.Hooks{BeforeDelete: func(context.Context, string) error {
		faultinject.CrashAt(faultinject.PointConsumerAfterCommitBeforeDelete)
		return nil
	}},
	outboxpub.Hooks{AfterPublish: func(context.Context, app.OutboxRecord) error {
		faultinject.CrashAt(faultinject.PointOutboxAfterPublishBeforeConfirm)
		return nil
	}},
)
