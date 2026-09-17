package awsclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws"
)

type Settings struct {
	Region          string
	EndpointURL     string
	AccessKeyID     string
	SecretAccessKey string
	HTTPClient      *http.Client
}

func NewHTTPClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}
	return &http.Client{Transport: transport.Clone()}
}

func CloseIdle(client *http.Client) {
	if client != nil {
		client.CloseIdleConnections()
	}
}

func LoadConfig(ctx context.Context, s Settings) (aws.Config, error) {
	if s.Region == "" {
		return aws.Config{}, errors.New("awsclient: region is required")
	}
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(s.Region)}
	if s.HTTPClient != nil {
		options = append(options, awsconfig.WithHTTPClient(s.HTTPClient))
	}
	if s.AccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(s.AccessKeyID, s.SecretAccessKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("awsclient: load config: %w", err)
	}
	otelaws.AppendMiddlewares(&cfg.APIOptions)
	return cfg, nil
}

func NewSQS(cfg aws.Config, endpointURL string) *sqs.Client {
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if endpointURL != "" {
			o.BaseEndpoint = aws.String(endpointURL)
		}
	})
}

func NewSNS(cfg aws.Config, endpointURL string) *sns.Client {
	return sns.NewFromConfig(cfg, func(o *sns.Options) {
		if endpointURL != "" {
			o.BaseEndpoint = aws.String(endpointURL)
		}
	})
}

type QueueAttributesGetter interface {
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

func QueueHealth(client QueueAttributesGetter, queueURL string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(queueURL),
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			return fmt.Errorf("sqs queue %s: %w", queueURL, err)
		}
		return nil
	}
}

type TopicAttributesGetter interface {
	GetTopicAttributes(ctx context.Context, in *sns.GetTopicAttributesInput, optFns ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error)
}

func TopicHealth(client TopicAttributesGetter, topicARN string) func(context.Context) error {
	return func(ctx context.Context) error {
		if _, err := client.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: aws.String(topicARN)}); err != nil {
			return fmt.Errorf("sns topic %s: %w", topicARN, err)
		}
		return nil
	}
}
