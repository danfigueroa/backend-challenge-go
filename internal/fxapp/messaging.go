package fxapp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/awsclient"
	"github.com/danfigueroa/backend-challenge-go/internal/adapter/sqsconsumer"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/worker"
	"github.com/danfigueroa/backend-challenge-go/internal/worker/outboxpub"
)

const dependencyReadyInterval = 500 * time.Millisecond

type ConsumerRunner struct {
	*worker.Runner
}

type OutboxRunner struct {
	*worker.Runner
}

var ConsumerModule = fx.Module("sqs-consumer",
	fx.Provide(NewConsumerSQSClient, NewConsumerRunner),
	fx.Invoke(func(*ConsumerRunner) {}),
)

var OutboxModule = fx.Module("outbox-publisher",
	fx.Provide(NewPublisherSNSClient, NewOutboxRunner),
	fx.Invoke(func(*OutboxRunner) {}),
)

func NewConsumerSQSClient(lc fx.Lifecycle, cfg config.Config) (*sqs.Client, error) {
	httpClient := awsclient.NewHTTPClient()
	lc.Append(fx.StopHook(func() { awsclient.CloseIdle(httpClient) }))
	awsCfg, err := awsclient.LoadConfig(context.Background(), awsclient.Settings{
		Region: cfg.AWS.Region, EndpointURL: cfg.AWS.EndpointURL, HTTPClient: httpClient,
		AccessKeyID: cfg.AWS.ConsumerAccessKeyID, SecretAccessKey: cfg.AWS.ConsumerSecretAccessKey,
	})
	if err != nil {
		return nil, err
	}
	return awsclient.NewSQS(awsCfg, cfg.AWS.EndpointURL), nil
}

func NewPublisherSNSClient(lc fx.Lifecycle, cfg config.Config) (*sns.Client, error) {
	httpClient := awsclient.NewHTTPClient()
	lc.Append(fx.StopHook(func() { awsclient.CloseIdle(httpClient) }))
	awsCfg, err := awsclient.LoadConfig(context.Background(), awsclient.Settings{
		Region: cfg.AWS.Region, EndpointURL: cfg.AWS.EndpointURL, HTTPClient: httpClient,
		AccessKeyID: cfg.AWS.PublisherAccessKeyID, SecretAccessKey: cfg.AWS.PublisherSecretAccessKey,
	})
	if err != nil {
		return nil, err
	}
	return awsclient.NewSNS(awsCfg, cfg.AWS.EndpointURL), nil
}

type consumerDeps struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    config.Config
	Client    *sqs.Client
	Wagering  *wageringapp.Service
	Checker   *health.Checker
	Metrics   *metrics.Metrics
	Logger    *slog.Logger
	Hooks     sqsconsumer.Hooks `optional:"true"`
}

func NewConsumerRunner(d consumerDeps) (*ConsumerRunner, error) {
	c := d.Config.Consumer
	consumer, err := sqsconsumer.New(d.Client, d.Wagering, sqsconsumer.Settings{
		ConsumerName: c.Name, QueueURL: c.QueueURL, DeadLetterURL: c.DeadLetterURL, Workers: c.Workers,
		MaxMessages: c.MaxMessages, WaitTime: c.WaitTime, ProcessingTimeout: c.ProcessingTimeout,
		RetryBaseDelay: c.RetryBaseDelay, RetryMaxDelay: c.RetryMaxDelay, ErrorBackoff: c.ErrorBackoff,
	}, d.Hooks, d.Metrics, d.Logger)
	if err != nil {
		return nil, err
	}

	queueCheck := awsclient.QueueHealth(d.Client, c.QueueURL)
	d.Checker.Register("sqs", queueCheck)
	runner := worker.NewRunner(consumer, d.Logger)
	d.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := waitDependency(ctx, "sqs input queue", d.Config.AWS.HealthTimeout, queueCheck); err != nil {
				return err
			}
			if err := waitDependency(ctx, "sqs dead-letter queue", d.Config.AWS.HealthTimeout, awsclient.QueueHealth(d.Client, c.DeadLetterURL)); err != nil {
				return err
			}
			return runner.Start(ctx)
		},
		OnStop: runner.Stop,
	})
	return &ConsumerRunner{Runner: runner}, nil
}

type outboxDeps struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    config.Config
	Client    *sns.Client
	Store     app.OutboxRepository
	Clock     app.Clock
	Checker   *health.Checker
	Metrics   *metrics.Metrics
	Logger    *slog.Logger
	Hooks     outboxpub.Hooks `optional:"true"`
}

func NewOutboxRunner(d outboxDeps) (*OutboxRunner, error) {
	o := d.Config.Outbox
	publisher, err := outboxpub.New(d.Client, d.Store, d.Clock, outboxpub.Settings{
		Owner: d.Config.App.InstanceID, TopicARN: o.TopicARN, PollInterval: o.PollInterval, ErrorBackoff: o.ErrorBackoff,
		BatchSize: o.BatchSize, Lease: o.Lease, PublishTimeout: o.PublishTimeout,
		RetryBaseDelay: o.RetryBaseDelay, RetryMaxDelay: o.RetryMaxDelay,
	}, d.Hooks, d.Metrics, d.Logger)
	if err != nil {
		return nil, err
	}

	topicCheck := awsclient.TopicHealth(d.Client, o.TopicARN)
	d.Checker.Register("sns", topicCheck)
	runner := worker.NewRunner(publisher, d.Logger)
	d.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := waitDependency(ctx, "sns events topic", d.Config.AWS.HealthTimeout, topicCheck); err != nil {
				return err
			}
			return runner.Start(ctx)
		},
		OnStop: runner.Stop,
	})
	return &OutboxRunner{Runner: runner}, nil
}

func waitDependency(ctx context.Context, name string, attemptTimeout time.Duration, check func(context.Context) error) error {
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := check(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(dependencyReadyInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s not ready: %w (last error: %w)", name, ctx.Err(), err)
		case <-timer.C:
		}
	}
}
