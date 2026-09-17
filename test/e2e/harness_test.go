//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/awsclient"
)

const (
	region            = "us-east-1"
	account           = "000000000000"
	eventuallyTimeout = 30 * time.Second
)

var (
	postgresHost   = envOr("E2E_POSTGRES_HOST", "localhost:55432")
	ownerUser      = envOr("E2E_POSTGRES_OWNER", "wallet_owner")
	ownerPassword  = envOr("E2E_POSTGRES_OWNER_PASSWORD", "local-owner-password")
	appUser        = envOr("E2E_POSTGRES_APP_USER", "wallet_service")
	appPassword    = envOr("E2E_POSTGRES_APP_PASSWORD", "local-service-password")
	keycloakURL    = envOr("E2E_KEYCLOAK_URL", "http://localhost:8081")
	localstackURL  = envOr("E2E_LOCALSTACK_URL", "http://localhost:4566")
	composeAPIURLs = strings.Split(envOr("E2E_COMPOSE_API_URLS", "http://localhost:8091,http://localhost:8092,http://localhost:8093"), ",")

	binary     string
	admin      *pgxpool.Pool
	sqsClient  *sqs.Client
	snsClient  *sns.Client
	httpClient = &http.Client{Timeout: 30 * time.Second}
	sequence   atomic.Int64
	tokens     sync.Map
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := setup(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup failed: %v\nstart the infrastructure first: docker compose up -d --wait postgres keycloak localstack\n", err)
		return 1
	}
	defer admin.Close()
	defer os.RemoveAll(filepath.Dir(binary))
	return m.Run()
}

func setup(ctx context.Context) error {
	var err error
	admin, err = pgxpool.New(ctx, fmt.Sprintf("postgres://%s:%s@%s/postgres?sslmode=disable", ownerUser, ownerPassword, postgresHost))
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if err := admin.Ping(ctx); err != nil {
		return fmt.Errorf("postgres not reachable at %s: %w", postgresHost, err)
	}
	if _, err := token(ctx, "wallet-internal"); err != nil {
		return fmt.Errorf("keycloak not reachable at %s: %w", keycloakURL, err)
	}

	cfg, err := awsclient.LoadConfig(ctx, awsclient.Settings{Region: region, AccessKeyID: "test", SecretAccessKey: "test"})
	if err != nil {
		return err
	}
	sqsClient, snsClient = awsclient.NewSQS(cfg, localstackURL), awsclient.NewSNS(cfg, localstackURL)
	if _, err := sqsClient.ListQueues(ctx, &sqs.ListQueuesInput{}); err != nil {
		return fmt.Errorf("localstack not reachable at %s: %w", localstackURL, err)
	}

	dir, err := os.MkdirTemp("", "wallet-e2e-")
	if err != nil {
		return err
	}
	binary = filepath.Join(dir, "wallet")
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	build := exec.CommandContext(ctx, "go", "build", "-tags", "faultinject", "-o", binary, "./cmd/wallet")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build service binary: %w\n%s", err, out)
	}
	return nil
}

func token(ctx context.Context, client string) (string, error) {
	if cached, ok := tokens.Load(client); ok {
		entry := cached.(cachedToken)
		if time.Until(entry.expires) > 30*time.Second {
			return entry.value, nil
		}
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {client}, "client_secret": {client + "-local-secret"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keycloakURL+"/realms/wagering/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("token for %s: status %d: %w", client, resp.StatusCode, err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("token for %s: status %d without access token", client, resp.StatusCode)
	}
	tokens.Store(client, cachedToken{value: body.AccessToken, expires: time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)})
	return body.AccessToken, nil
}

type cachedToken struct {
	value   string
	expires time.Time
}

func mustToken(t *testing.T, client string) string {
	t.Helper()
	tok, err := token(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type Env struct {
	t        *testing.T
	name     string
	ownerDSN string
	appDSN   string
	db       *pgxpool.Pool
	queueURL string
	dlqURL   string
	topicARN string
	auditURL string
}

func newEnv(t *testing.T) *Env {
	t.Helper()
	ctx := context.Background()
	n := sequence.Add(1)
	name := fmt.Sprintf("e2e_%d_%d", os.Getpid(), n)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	e := &Env{
		t:        t,
		name:     name,
		ownerDSN: fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", ownerUser, ownerPassword, postgresHost, name),
		appDSN:   fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", appUser, appPassword, postgresHost, name),
	}
	t.Cleanup(func() {
		if e.db != nil {
			e.db.Close()
		}
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	migrate := exec.CommandContext(ctx, binary, "migrate", "up")
	migrate.Env = []string{"DATABASE_MIGRATIONS_URL=" + e.ownerDSN}
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	var err error
	if e.db, err = pgxpool.New(ctx, e.ownerDSN); err != nil {
		t.Fatal(err)
	}

	queue, dlq := fmt.Sprintf("e2e-%s-in.fifo", name), fmt.Sprintf("e2e-%s-dlq.fifo", name)
	dlqOut, err := sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(dlq), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	redrive, _ := json.Marshal(map[string]string{"deadLetterTargetArn": fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, account, dlq), "maxReceiveCount": "5"})
	queueOut, err := sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queue), Attributes: map[string]string{
		"FifoQueue": "true", "VisibilityTimeout": "5", "RedrivePolicy": string(redrive),
	}})
	if err != nil {
		t.Fatal(err)
	}
	topic, err := snsClient.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String(fmt.Sprintf("e2e-%s-events.fifo", name)), Attributes: map[string]string{"FifoTopic": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	audit := fmt.Sprintf("e2e-%s-audit.fifo", name)
	auditOut, err := sqsClient.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(audit), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snsClient.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, account, audit)),
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	}); err != nil {
		t.Fatal(err)
	}
	e.queueURL, e.dlqURL, e.topicARN, e.auditURL = aws.ToString(queueOut.QueueUrl), aws.ToString(dlqOut.QueueUrl), aws.ToString(topic.TopicArn), aws.ToString(auditOut.QueueUrl)
	return e
}

type Instance struct {
	name     string
	api      string
	adminURL string
	cmd      *exec.Cmd
	logs     *lockedBuffer
	done     chan struct{}
	exitCode atomic.Int32
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func (e *Env) start(name, roles string, extra map[string]string) *Instance {
	e.t.Helper()
	inst := e.launch(name, roles, extra)
	inst.waitReady(e.t)
	return inst
}

func (e *Env) launch(name, roles string, extra map[string]string) *Instance {
	e.t.Helper()
	apiPort, adminPort := freePort(e.t), freePort(e.t)
	env := map[string]string{
		"HOME": os.Getenv("HOME"), "APP_INSTANCE_ID": name, "APP_ROLES": roles, "LOG_LEVEL": "info",
		"HTTP_ADDR": "127.0.0.1:" + apiPort, "ADMIN_ADDR": "127.0.0.1:" + adminPort, "APP_SHUTDOWN_TIMEOUT": "15s",
		"DATABASE_URL": e.appDSN, "DATABASE_MAX_CONNS": "10", "DATABASE_MIN_CONNS": "0",
		"AUTH_ISSUER": "http://localhost:8081/realms/wagering", "AUTH_JWKS_URL": keycloakURL + "/realms/wagering/protocol/openid-connect/certs",
		"AWS_REGION": region, "AWS_ENDPOINT_URL": localstackURL,
		"SQS_CONSUMER_ACCESS_KEY_ID": "wager-transactions-consumer", "SQS_CONSUMER_SECRET_ACCESS_KEY": "local-consumer-secret",
		"SNS_PUBLISHER_ACCESS_KEY_ID": "wallet-events-publisher", "SNS_PUBLISHER_SECRET_ACCESS_KEY": "local-publisher-secret",
		"SQS_INPUT_QUEUE_URL": e.queueURL, "SQS_DLQ_URL": e.dlqURL, "SNS_EVENTS_TOPIC_ARN": e.topicARN,
		"SQS_WAIT_TIME": "1s", "SQS_PROCESSING_TIMEOUT": "4s", "SQS_RETRY_BASE_DELAY": "1s", "SQS_RETRY_MAX_DELAY": "2s",
		"OUTBOX_POLL_INTERVAL": "100ms", "OUTBOX_ERROR_BACKOFF": "500ms", "OUTBOX_LEASE": "3s", "OUTBOX_PUBLISH_TIMEOUT": "1s",
		"PENDING_POLL_INTERVAL": "100ms", "PENDING_ERROR_BACKOFF": "500ms", "PENDING_CLAIM_LEASE": "3s", "PENDING_ITERATION_TIMEOUT": "2s",
		"PENDING_BASE_DELAY": "200ms", "PENDING_MAX_DELAY": "1s",
	}
	for k, v := range extra {
		env[k] = v
	}

	cmd := exec.CommandContext(context.Background(), binary, "serve")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	inst := &Instance{name: name, api: "http://127.0.0.1:" + apiPort, adminURL: "http://127.0.0.1:" + adminPort, cmd: cmd, logs: &lockedBuffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = inst.logs, inst.logs
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		err := cmd.Wait()
		code := 0
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exitErr.ExitCode()
		}
		inst.exitCode.Store(int32(code))
		close(inst.done)
	}()
	e.t.Cleanup(func() {
		inst.kill()
		if e.t.Failed() {
			e.t.Logf("logs of %s:\n%s", name, tail(inst.logs.String(), 60))
		}
	})

	return inst
}

func (i *Instance) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case <-i.done:
			t.Fatalf("%s exited during startup (code %d):\n%s", i.name, i.exitCode.Load(), i.logs.String())
		default:
		}
		if r := call(t, http.MethodGet, i.adminURL+"/health/ready", "", ""); r.status == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready:\n%s", i.name, i.logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func tail(s string, lines int) string {
	parts := strings.Split(s, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func (i *Instance) kill() {
	if i.cmd.Process != nil {
		_ = i.cmd.Process.Signal(syscall.SIGKILL)
	}
	<-i.done
}

func (i *Instance) terminate(t *testing.T) {
	t.Helper()
	_ = i.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-i.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not stop after SIGTERM", i.name)
	}
	if code := i.exitCode.Load(); code != 0 {
		t.Errorf("%s exited with %d after SIGTERM:\n%s", i.name, code, tail(i.logs.String(), 30))
	}
}

func (i *Instance) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-i.done:
		return int(i.exitCode.Load())
	case <-time.After(timeout):
		t.Fatalf("%s still running after %v", i.name, timeout)
		return 0
	}
}

type reply struct {
	status int
	body   map[string]any
	raw    string
}

func call(t *testing.T, method, url, bearer, body string, headers ...string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return reply{status: 0, raw: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

type wallet struct {
	ID       string
	PlayerID string
}

func openWallet(t *testing.T, api, amount string) wallet {
	t.Helper()
	player := newUUID(t)
	r := call(t, http.MethodPost, api+"/wallets", mustToken(t, "wallet-internal"),
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, player, amount))
	if r.status != http.StatusCreated {
		t.Fatalf("open wallet on %s: %d %s", api, r.status, r.raw)
	}
	return wallet{ID: r.body["id"].(string), PlayerID: player}
}

func newUUID(t *testing.T) string {
	t.Helper()
	var id string
	if err := admin.QueryRow(context.Background(), "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func operationBody(w wallet, kind, externalID, amount, reference string) string {
	body := map[string]any{
		"providerId": "provider-a", "externalTransactionId": externalID, "playerId": w.PlayerID, "walletId": w.ID,
		"roundId": "round-e2e", "gameId": "game-e2e", "kind": kind, "money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if reference != "" {
		body["referenceExternalTransactionId"] = reference
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func submit(t *testing.T, api string, w wallet, kind, externalID, amount, reference string) reply {
	t.Helper()
	return call(t, http.MethodPost, api+"/wagering/transactions", mustToken(t, "provider-a"),
		operationBody(w, kind, externalID, amount, reference), "Idempotency-Key", "provider-a:"+externalID)
}

func sqsBet(w wallet, messageID, externalID, amount string) string {
	var data map[string]any
	_ = json.Unmarshal([]byte(operationBody(w, "BET", externalID, amount, "")), &data)
	data["idempotencyKey"] = "provider-a:" + externalID
	body, _ := json.Marshal(map[string]any{
		"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": data,
	})
	return string(body)
}

func (e *Env) send(t *testing.T, w wallet, messageID, body string) {
	t.Helper()
	if _, err := sqsClient.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(e.queueURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(messageID),
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *Env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e *Env) queueDepth(t *testing.T, queueURL string) int {
	t.Helper()
	out, err := sqsClient.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, _ := strconv.Atoi(out.Attributes["ApproximateNumberOfMessages"])
	hidden, _ := strconv.Atoi(out.Attributes["ApproximateNumberOfMessagesNotVisible"])
	return visible + hidden
}

func (e *Env) auditEvents(t *testing.T, want int, timeout time.Duration) map[string]int {
	t.Helper()
	seen := map[string]int{}
	deadline := time.Now().Add(timeout)
	quiet := 0
	for time.Now().Before(deadline) {
		out, err := sqsClient.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(e.auditURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			var envelope struct {
				EventID string `json:"eventId"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &envelope)
			seen[envelope.EventID]++
			_, _ = sqsClient.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(e.auditURL), ReceiptHandle: m.ReceiptHandle})
		}
		if len(out.Messages) == 0 {
			quiet++
		} else {
			quiet = 0
		}
		if len(seen) >= want && quiet >= 2 {
			break
		}
	}
	return seen
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(eventuallyTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", eventuallyTimeout, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (e *Env) assertFinancialConsistency(t *testing.T) {
	t.Helper()
	rows, err := e.db.Query(context.Background(), `
		SELECT w.id::text, w.balance_minor, w.version,
		       COALESCE(sum(CASE WHEN l.direction = 'CREDIT' THEN l.amount_minor ELSE -l.amount_minor END), 0)::BIGINT,
		       COALESCE(max(l.wallet_version), 0)
		FROM wallets w LEFT JOIN ledger_entries l ON l.wallet_id = w.id
		GROUP BY w.id, w.balance_minor, w.version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var (
			id                          string
			balance, version, net, last int64
		)
		if err := rows.Scan(&id, &balance, &version, &net, &last); err != nil {
			t.Fatal(err)
		}
		checked++
		if balance != net || balance < 0 || (last != 0 && last != version) {
			t.Errorf("wallet %s inconsistent: balance=%d ledger=%d version=%d lastEntry=%d", id, balance, net, version, last)
		}
	}
	if n := e.count(t, "SELECT count(*) FROM (SELECT transaction_id FROM ledger_entries GROUP BY transaction_id HAVING count(*) > 1) d"); n != 0 {
		t.Errorf("%d transactions moved money more than once", n)
	}
	t.Logf("financial consistency verified for %d wallets", checked)
}
