package sqsconsumer

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
)

type noopSQS struct{}

func (noopSQS) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	return &sqs.ReceiveMessageOutput{}, nil
}

func (noopSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	return &sqs.DeleteMessageOutput{}, nil
}

func (noopSQS) ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (noopSQS) SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	return &sqs.SendMessageOutput{}, nil
}

type noopProcessor struct{}

func (noopProcessor) Process(context.Context, wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
	return wageringapp.ProcessResult{}, nil
}
