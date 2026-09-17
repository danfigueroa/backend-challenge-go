//go:build integration

package lstest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/awsclient"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

const (
	Image   = "localstack/localstack:4.14.0"
	Region  = "us-east-1"
	Account = "000000000000"
)

type Instance struct {
	container testcontainers.Container
	Endpoint  string
	SQS       *sqs.Client
	SNS       *sns.Client
	http      *http.Client
	sequence  atomic.Int64
}

func Start(ctx context.Context) (*Instance, error) {
	root := pgtest.RepoRoot()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"4566/tcp"},
			Env:          map[string]string{"SERVICES": "sqs,sns,iam,sts", "AWS_DEFAULT_REGION": Region},
			Files: []testcontainers.ContainerFile{
				{HostFilePath: filepath.Join(root, "deploy/localstack/init/ready.d/01-provision-messaging.sh"), ContainerFilePath: "/etc/localstack/init/ready.d/01-provision-messaging.sh", FileMode: 0o755},
				{HostFilePath: filepath.Join(root, "deploy/localstack/policies/wager-transactions-producer.json"), ContainerFilePath: "/etc/localstack/policies/wager-transactions-producer.json", FileMode: 0o644},
				{HostFilePath: filepath.Join(root, "deploy/localstack/policies/wager-transactions-consumer.json"), ContainerFilePath: "/etc/localstack/policies/wager-transactions-consumer.json", FileMode: 0o644},
				{HostFilePath: filepath.Join(root, "deploy/localstack/policies/wallet-events-publisher.json"), ContainerFilePath: "/etc/localstack/policies/wallet-events-publisher.json", FileMode: 0o644},
			},
			WaitingFor: wait.ForHTTP("/_localstack/init/ready").WithPort("4566/tcp").
				WithResponseMatcher(func(body io.Reader) bool {
					var status struct {
						Completed bool `json:"completed"`
					}
					return json.NewDecoder(body).Decode(&status) == nil && status.Completed
				}).WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("start localstack: %w", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "4566/tcp", "http")
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, fmt.Errorf("localstack endpoint: %w", err)
	}
	httpClient := awsclient.NewHTTPClient()
	cfg, err := awsclient.LoadConfig(ctx, awsclient.Settings{Region: Region, AccessKeyID: "test", SecretAccessKey: "test", HTTPClient: httpClient})
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, err
	}
	return &Instance{
		container: container, Endpoint: endpoint, http: httpClient,
		SQS: awsclient.NewSQS(cfg, endpoint), SNS: awsclient.NewSNS(cfg, endpoint),
	}, nil
}

func (i *Instance) CloseIdleConnections() {
	awsclient.CloseIdle(i.http)
}

func (i *Instance) Terminate(ctx context.Context) error {
	return testcontainers.TerminateContainer(i.container, testcontainers.StopContext(ctx))
}

func (i *Instance) QueueURL(name string) string {
	return i.Endpoint + "/" + Account + "/" + name
}

type QueuePair struct {
	URL           string
	DeadLetterURL string
}

func (i *Instance) CreateQueuePair(t *testing.T, visibility time.Duration, maxReceiveCount int) QueuePair {
	t.Helper()
	ctx := context.Background()
	n := i.sequence.Add(1)
	dlqName := fmt.Sprintf("test-%d-dlq.fifo", n)
	queueName := fmt.Sprintf("test-%d.fifo", n)

	dlq, err := i.SQS.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(dlqName), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	redrive, _ := json.Marshal(map[string]string{
		"deadLetterTargetArn": fmt.Sprintf("arn:aws:sqs:%s:%s:%s", Region, Account, dlqName),
		"maxReceiveCount":     strconv.Itoa(maxReceiveCount),
	})
	queue, err := i.SQS.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queueName), Attributes: map[string]string{
		"FifoQueue": "true", "VisibilityTimeout": strconv.Itoa(int(visibility / time.Second)), "RedrivePolicy": string(redrive),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return QueuePair{URL: aws.ToString(queue.QueueUrl), DeadLetterURL: aws.ToString(dlq.QueueUrl)}
}

type Topic struct {
	ARN      string
	AuditURL string
}

func (i *Instance) CreateTopicWithAudit(t *testing.T) Topic {
	t.Helper()
	ctx := context.Background()
	n := i.sequence.Add(1)
	topic, err := i.SNS.CreateTopic(ctx, &sns.CreateTopicInput{
		Name:       aws.String(fmt.Sprintf("events-%d.fifo", n)),
		Attributes: map[string]string{"FifoTopic": "true", "ContentBasedDeduplication": "false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueName := fmt.Sprintf("events-audit-%d.fifo", n)
	queue, err := i.SQS.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queueName), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SNS.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"),
		Endpoint:   aws.String(fmt.Sprintf("arn:aws:sqs:%s:%s:%s", Region, Account, queueName)),
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	}); err != nil {
		t.Fatal(err)
	}
	return Topic{ARN: aws.ToString(topic.TopicArn), AuditURL: aws.ToString(queue.QueueUrl)}
}

func (i *Instance) Drain(t *testing.T, queueURL string, want int, timeout time.Duration) []types.Message {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var messages []types.Message
	for time.Now().Before(deadline) && (want < 0 || len(messages) < want) {
		out, err := i.SQS.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageAttributeNames:       []string{"All"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			messages = append(messages, m)
			if _, err := i.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return messages
}

func (i *Instance) Send(t *testing.T, queueURL, groupID, dedupID, body string, attributes map[string]string) {
	t.Helper()
	attrs := map[string]types.MessageAttributeValue{}
	for k, v := range attributes {
		attrs[k] = types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(v)}
	}
	if _, err := i.SQS.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(queueURL), MessageBody: aws.String(body), MessageGroupId: aws.String(groupID),
		MessageDeduplicationId: aws.String(dedupID), MessageAttributes: attrs,
	}); err != nil {
		t.Fatal(err)
	}
}

func (i *Instance) ApproximateMessages(t *testing.T, queueURL string) int {
	t.Helper()
	out, err := i.SQS.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, key := range []string{"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"} {
		n, _ := strconv.Atoi(strings.TrimSpace(out.Attributes[key]))
		total += n
	}
	return total
}
