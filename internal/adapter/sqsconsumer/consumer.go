package sqsconsumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/sync/errgroup"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/logging"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/worker"
)

const (
	AttributeCorrelationID = "correlationId"
	AttributeReason        = "dlqReason"
	AttributeErrorCode     = "dlqErrorCode"
	AttributeDetail        = "dlqDetail"
	AttributeReceiveCount  = "dlqReceiveCount"
	AttributeSourceQueue   = "dlqSourceQueue"

	ReasonMalformed  = "MALFORMED_MESSAGE"
	ReasonValidation = "VALIDATION_FAILED"
	ReasonConflict   = "IDEMPOTENCY_CONFLICT"
	ReasonInbox      = "MESSAGE_ID_REUSED"
	ReasonForbidden  = "FORBIDDEN"

	maxDetailLength = 256
)

type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, optFns ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

type Processor interface {
	Process(ctx context.Context, cmd wageringapp.ProcessCommand) (wageringapp.ProcessResult, error)
}

type Settings struct {
	ConsumerName      string
	QueueURL          string
	DeadLetterURL     string
	Workers           int
	MaxMessages       int32
	WaitTime          time.Duration
	ProcessingTimeout time.Duration
	RetryBaseDelay    time.Duration
	RetryMaxDelay     time.Duration
	ErrorBackoff      time.Duration
}

type Hooks struct {
	BeforeDelete func(ctx context.Context, messageID string) error
}

type Consumer struct {
	client    SQSAPI
	processor Processor
	settings  Settings
	hooks     Hooks
	metrics   *metrics.Metrics
	logger    *slog.Logger
	now       func() time.Time
}

func New(client SQSAPI, processor Processor, s Settings, hooks Hooks, m *metrics.Metrics, logger *slog.Logger) (*Consumer, error) {
	switch {
	case client == nil || processor == nil || m == nil || logger == nil:
		return nil, errors.New("sqsconsumer: all dependencies are required")
	case s.ConsumerName == "" || s.QueueURL == "" || s.DeadLetterURL == "":
		return nil, errors.New("sqsconsumer: consumer name, queue URL and dead-letter URL are required")
	case s.Workers < 1 || s.MaxMessages < 1 || s.MaxMessages > 10:
		return nil, errors.New("sqsconsumer: workers must be positive and max messages within [1, 10]")
	case s.ProcessingTimeout <= 0 || s.RetryBaseDelay < time.Second || s.RetryMaxDelay < s.RetryBaseDelay || s.ErrorBackoff <= 0:
		return nil, errors.New("sqsconsumer: invalid timeouts or retry delays")
	}
	return &Consumer{
		client: client, processor: processor, settings: s, hooks: hooks, metrics: m,
		logger: logger.With(slog.String("worker", "sqs-consumer"), slog.String("consumer", s.ConsumerName)),
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

func (c *Consumer) Name() string { return "sqs-consumer:" + c.settings.ConsumerName }

func (c *Consumer) Run(ctx context.Context) error {
	var g errgroup.Group
	for range c.settings.Workers {
		g.Go(func() error { return c.poll(ctx) })
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("sqs consumer: %w", err)
	}
	return nil
}

func (c *Consumer) poll(ctx context.Context) error {
	return worker.Loop(ctx, worker.LoopSettings{
		Interval:   10 * time.Millisecond,
		MaxBackoff: c.settings.ErrorBackoff,
		OnError: func(err error) {
			c.metrics.WorkerErrors.WithLabelValues("sqs-consumer").Inc()
			c.logger.WarnContext(ctx, "sqs receive failed", slog.Any("error", err))
		},
	}, func(ctx context.Context) (bool, error) {
		out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:                    aws.String(c.settings.QueueURL),
			MaxNumberOfMessages:         c.settings.MaxMessages,
			WaitTimeSeconds:             seconds32(c.settings.WaitTime),
			MessageAttributeNames:       []string{"All"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount, types.MessageSystemAttributeNameMessageGroupId},
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, fmt.Errorf("receive interrupted: %w", ctxErr)
			}
			return false, fmt.Errorf("receive: %w", err)
		}
		for i, msg := range out.Messages {
			if ctxErr := ctx.Err(); ctxErr != nil {
				c.release(ctx, out.Messages[i:])
				return false, fmt.Errorf("consumer stopping: %w", ctxErr)
			}
			c.handle(ctx, msg)
		}
		return len(out.Messages) > 0, nil
	})
}

type outcome int

const (
	outcomeDelete outcome = iota
	outcomeDeadLetter
	outcomeRetry
)

func (c *Consumer) handle(parent context.Context, msg types.Message) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.settings.ProcessingTimeout)
	defer cancel()

	sqsID := aws.ToString(msg.MessageId)
	receiveCount := receiveCountOf(msg)
	ctx = otel.GetTextMapPropagator().Extract(ctx, messageCarrier(msg.MessageAttributes))

	decoded, err := Decode(aws.ToString(msg.Body))
	if err != nil {
		reason, code := classifyDecodeError(err)
		ctx = logging.With(ctx, slog.String("sqsMessageId", sqsID))
		c.deadLetter(ctx, msg, reason, code, err, receiveCount)
		return
	}

	correlationID := attributeString(msg.MessageAttributes, AttributeCorrelationID)
	if correlationID == "" {
		correlationID = decoded.MessageID
	}
	ctx = logging.With(ctx,
		slog.String(logging.KeyMessageID, decoded.MessageID),
		slog.String(logging.KeyCorrelationID, correlationID),
		slog.String(logging.KeyProviderID, decoded.Input.ProviderID),
		slog.String(logging.KeyWalletID, decoded.Input.WalletID),
	)

	result, err := c.processor.Process(ctx, wageringapp.ProcessCommand{
		Actor: app.BrokerActor(c.settings.ConsumerName),
		Meta:  app.Metadata{Channel: app.ChannelSQS, CorrelationID: correlationID, CausationID: decoded.MessageID},
		Input: decoded.Input,
		Delivery: &wageringapp.Delivery{
			ConsumerName: c.settings.ConsumerName, MessageID: decoded.MessageID,
			PayloadHash: decoded.PayloadHash, ReceivedAt: c.now(),
		},
	})

	switch decision, reason, code := classifyProcessError(err); decision {
	case outcomeDelete:
		ctx = logging.With(ctx, slog.String(logging.KeyTransactionID, result.Transaction.ID().String()))
		c.complete(ctx, msg, result)
	case outcomeDeadLetter:
		c.deadLetter(ctx, msg, reason, code, err, receiveCount)
	case outcomeRetry:
		c.retry(ctx, msg, receiveCount, err)
	}
}

func (c *Consumer) complete(ctx context.Context, msg types.Message, result wageringapp.ProcessResult) {
	label := string(result.Transaction.Status())
	switch {
	case result.DuplicateDelivery:
		label = "DUPLICATE_DELIVERY"
	case result.IdempotentReplay:
		label = "IDEMPOTENT_REPLAY"
	}

	if c.hooks.BeforeDelete != nil {
		if err := c.hooks.BeforeDelete(ctx, aws.ToString(msg.MessageId)); err != nil {
			c.logger.WarnContext(ctx, "message left in queue by hook", slog.Any("error", err))
			return
		}
	}
	if err := c.delete(ctx, msg); err != nil {
		c.metrics.SQSMessagesTotal.WithLabelValues(c.settings.ConsumerName, "DELETE_FAILED").Inc()
		c.logger.WarnContext(ctx, "message processed but not deleted; it will be redelivered and answered from the inbox", slog.Any("error", err))
		return
	}
	c.metrics.SQSMessagesTotal.WithLabelValues(c.settings.ConsumerName, label).Inc()
	c.logger.InfoContext(ctx, "sqs message handled", slog.String("result", label), slog.String("status", string(result.Transaction.Status())))
}

func (c *Consumer) retry(ctx context.Context, msg types.Message, receiveCount int, cause error) {
	delay := c.retryDelay(receiveCount)
	c.metrics.SQSMessagesTotal.WithLabelValues(c.settings.ConsumerName, "RETRY").Inc()
	c.metrics.RetriesTotal.WithLabelValues("sqs_message", "transient").Inc()
	c.logger.WarnContext(ctx, "transient failure; message will be retried",
		slog.Int("receiveCount", receiveCount), slog.Duration("visibilityDelay", delay), slog.Any("error", cause))

	_, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.settings.QueueURL),
		ReceiptHandle:     msg.ReceiptHandle,
		VisibilityTimeout: seconds32(delay),
	})
	if err != nil {
		c.logger.WarnContext(ctx, "could not change visibility; queue default applies", slog.Any("error", err))
	}
}

func (c *Consumer) retryDelay(receiveCount int) time.Duration {
	delay := c.settings.RetryBaseDelay
	for i := 1; i < receiveCount && delay < c.settings.RetryMaxDelay; i++ {
		delay *= 2
	}
	return min(delay, c.settings.RetryMaxDelay)
}

func (c *Consumer) deadLetter(ctx context.Context, msg types.Message, reason, code string, cause error, receiveCount int) {
	detail := cause.Error()
	if len(detail) > maxDetailLength {
		detail = detail[:maxDetailLength]
	}
	groupID := attributeSystem(msg, types.MessageSystemAttributeNameMessageGroupId)
	if groupID == "" {
		groupID = "dead-letter"
	}
	_, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.settings.DeadLetterURL),
		MessageBody:            msg.Body,
		MessageGroupId:         aws.String(groupID),
		MessageDeduplicationId: msg.MessageId,
		MessageAttributes: map[string]types.MessageAttributeValue{
			AttributeReason:       stringAttribute(reason),
			AttributeErrorCode:    stringAttribute(code),
			AttributeDetail:       stringAttribute(detail),
			AttributeReceiveCount: {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(receiveCount))},
			AttributeSourceQueue:  stringAttribute(c.settings.QueueURL),
		},
	})
	if err != nil {
		c.metrics.SQSMessagesTotal.WithLabelValues(c.settings.ConsumerName, "DEAD_LETTER_FAILED").Inc()
		c.logger.ErrorContext(ctx, "could not dead-letter message; it stays in the queue until redrive", slog.Any("error", err), slog.String("reason", reason))
		return
	}
	if err := c.delete(ctx, msg); err != nil {
		c.logger.WarnContext(ctx, "dead-lettered message could not be deleted from the source queue", slog.Any("error", err))
	}
	c.metrics.SQSDeadLettered.WithLabelValues(c.settings.ConsumerName, reason).Inc()
	c.metrics.SQSMessagesTotal.WithLabelValues(c.settings.ConsumerName, "DEAD_LETTERED").Inc()
	c.logger.WarnContext(ctx, "message sent to dead-letter queue", slog.String("reason", reason), slog.String("errorCode", code), slog.String("detail", detail))
}

func (c *Consumer) delete(ctx context.Context, msg types.Message) error {
	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.settings.QueueURL), ReceiptHandle: msg.ReceiptHandle})
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

func (c *Consumer) release(parent context.Context, messages []types.Message) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	for _, msg := range messages {
		_, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl: aws.String(c.settings.QueueURL), ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 0,
		})
		if err != nil {
			c.logger.WarnContext(ctx, "could not release message during shutdown; it becomes visible after its timeout", slog.Any("error", err))
			continue
		}
		c.metrics.SQSMessagesTotal.WithLabelValues(c.settings.ConsumerName, "RELEASED").Inc()
	}
	c.logger.InfoContext(ctx, "released unprocessed messages for redelivery", slog.Int("count", len(messages)))
}

const maxVisibilitySeconds = 12 * 60 * 60

func seconds32(d time.Duration) int32 {
	s := d / time.Second
	switch {
	case s < 0:
		return 0
	case s > maxVisibilitySeconds:
		return maxVisibilitySeconds
	default:
		return int32(s)
	}
}

func classifyDecodeError(err error) (reason, code string) {
	if verr, ok := errors.AsType[*wagering.ValidationError](err); ok {
		return ReasonValidation, string(verr.Code)
	}
	return ReasonMalformed, string(wagering.CodeMalformedRequest)
}

func classifyProcessError(err error) (outcome, string, string) {
	if err == nil {
		return outcomeDelete, "", ""
	}
	if verr, ok := errors.AsType[*wagering.ValidationError](err); ok {
		return outcomeDeadLetter, ReasonValidation, string(verr.Code)
	}
	if conflict, ok := errors.AsType[*wageringapp.IdempotencyConflictError](err); ok {
		return outcomeDeadLetter, ReasonConflict, string(conflict.Code)
	}
	switch {
	case errors.Is(err, app.ErrInboxMismatch):
		return outcomeDeadLetter, ReasonInbox, ReasonInbox
	case errors.Is(err, app.ErrForbidden):
		return outcomeDeadLetter, ReasonForbidden, ReasonForbidden
	}
	return outcomeRetry, "", ""
}

func receiveCountOf(msg types.Message) int {
	n, err := strconv.Atoi(attributeSystem(msg, types.MessageSystemAttributeNameApproximateReceiveCount))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func attributeSystem(msg types.Message, name types.MessageSystemAttributeName) string {
	return msg.Attributes[string(name)]
}

func attributeString(attrs map[string]types.MessageAttributeValue, name string) string {
	if v, ok := attrs[name]; ok {
		return aws.ToString(v.StringValue)
	}
	return ""
}

func stringAttribute(value string) types.MessageAttributeValue {
	if value == "" {
		value = "-"
	}
	return types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(value)}
}

type messageCarrier map[string]types.MessageAttributeValue

func (m messageCarrier) Get(key string) string {
	return attributeString(m, key)
}

func (m messageCarrier) Set(string, string) {}

func (m messageCarrier) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

var _ propagation.TextMapCarrier = messageCarrier(nil)
