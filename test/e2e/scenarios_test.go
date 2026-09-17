//go:build e2e

package e2e_test

import (
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/faultinject"
)

const allRoles = "api,consumer,outbox,pendingref"

func startCluster(e *Env, size int) []*Instance {
	instances := make([]*Instance, size)
	for i := range instances {
		instances[i] = e.start(fmt.Sprintf("node-%d", i+1), allRoles, nil)
	}
	return instances
}

func TestIdenticalBetAcrossInstancesDebitsOnce(t *testing.T) {
	e := newEnv(t)
	nodes := startCluster(e, 3)
	w := openWallet(t, nodes[0].api, "1000.00")

	var wg sync.WaitGroup
	var processed, replays atomic.Int32
	for i := range 50 {
		wg.Go(func() {
			r := submit(t, nodes[i%len(nodes)].api, w, "BET", "bet-concurrent", "25.00", "")
			if r.status != http.StatusOK {
				t.Errorf("unexpected response %d %s", r.status, r.raw)
				return
			}
			if r.body["balance"].(map[string]any)["amount"] != "975.00" {
				t.Errorf("replay returned a different balance: %s", r.raw)
			}
			if r.body["idempotentReplay"] == true {
				replays.Add(1)
			} else {
				processed.Add(1)
			}
		})
	}
	wg.Wait()

	if processed.Load() != 1 || replays.Load() != 49 {
		t.Fatalf("processed=%d replays=%d, want 1 and 49", processed.Load(), replays.Load())
	}
	if n := e.count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Fatalf("debits = %d, want 1", n)
	}
	e.assertFinancialConsistency(t)
}

func TestCompetingBetsAcrossInstancesNeverOverdraw(t *testing.T) {
	e := newEnv(t)
	nodes := startCluster(e, 3)
	w := openWallet(t, nodes[0].api, "100.00")

	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Go(func() {
			statuses[i] = submit(t, nodes[i+1].api, w, "BET", fmt.Sprintf("bet-80-%d", i), "80.00", "").status
		})
	}
	wg.Wait()

	slices.Sort(statuses)
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusUnprocessableEntity {
		t.Fatalf("statuses = %v, want one 200 and one 422", statuses)
	}
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 2000 {
		t.Fatalf("balance = %d, want 2000", n)
	}
	if n := e.count(t, "SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND failure_code = 'INSUFFICIENT_FUNDS'", w.ID); n != 1 {
		t.Fatalf("insufficient funds rejections = %d, want 1", n)
	}
	e.assertFinancialConsistency(t)
}

func TestManyWalletsUnderConcurrentLoadStayConsistent(t *testing.T) {
	e := newEnv(t)
	nodes := startCluster(e, 3)

	wallets := make([]wallet, 20)
	for i := range wallets {
		wallets[i] = openWallet(t, nodes[i%len(nodes)].api, "500.00")
	}

	var wg sync.WaitGroup
	for i, w := range wallets {
		for j := range 10 {
			wg.Go(func() {
				node := nodes[(i+j)%len(nodes)]
				id := fmt.Sprintf("load-%d-%d", i, j)
				if r := submit(t, node.api, w, "BET", id, "30.00", ""); r.status != http.StatusOK && r.status != http.StatusUnprocessableEntity {
					t.Errorf("bet %s: %d %s", id, r.status, r.raw)
				}
				if j%2 == 0 {
					if r := submit(t, node.api, w, "WIN", id+"-win", "10.00", id); r.status != http.StatusOK && r.status != http.StatusAccepted && r.status != http.StatusUnprocessableEntity {
						t.Errorf("win %s: %d %s", id, r.status, r.raw)
					}
				}
			})
		}
	}
	wg.Wait()

	eventually(t, "pending references to settle", func() bool {
		return e.count(t, "SELECT count(*) FROM wager_transactions WHERE status = 'PENDING_REFERENCE'") == 0
	})
	e.assertFinancialConsistency(t)
	for _, w := range wallets {
		r := call(t, http.MethodPost, nodes[0].api+"/wallets/"+w.ID+"/reconciliation", mustToken(t, "wallet-internal"), "")
		if r.status != http.StatusOK || r.body["consistent"] != true {
			t.Fatalf("reconciliation of %s: %d %s", w.ID, r.status, r.raw)
		}
	}
}

func TestSameOperationThroughHTTPAndSQSIsAppliedOnce(t *testing.T) {
	e := newEnv(t)
	nodes := startCluster(e, 2)
	w := openWallet(t, nodes[0].api, "200.00")

	for i := range 5 {
		e.send(t, w, fmt.Sprintf("msg-cross-%d", i), sqsBet(w, fmt.Sprintf("msg-cross-%d", i), "cross-channel", "40.00"))
	}
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			if r := submit(t, nodes[i%2].api, w, "BET", "cross-channel", "40.00", ""); r.status != http.StatusOK {
				t.Errorf("http: %d %s", r.status, r.raw)
			}
		})
	}
	wg.Wait()

	eventually(t, "input queue to drain", func() bool { return e.queueDepth(t, e.queueURL) == 0 })
	if n := e.count(t, "SELECT count(*) FROM inbox_messages WHERE processed_at IS NOT NULL"); n != 5 {
		t.Fatalf("processed inbox messages = %d, want 5", n)
	}
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 16000 {
		t.Fatalf("balance = %d, want 16000", n)
	}
	if n := e.queueDepth(t, e.dlqURL); n != 0 {
		t.Fatalf("dlq depth = %d, want 0", n)
	}
	e.assertFinancialConsistency(t)
}

func TestConsumerCrashAfterCommitBeforeDeleteIsRedeliveredWithoutDoubleDebit(t *testing.T) {
	e := newEnv(t)
	crashing := e.start("crashing-consumer", "consumer", map[string]string{faultinject.EnvCrashPoint: faultinject.PointConsumerAfterCommitBeforeDelete})
	api := e.start("api-only", "api,outbox", nil)
	w := openWallet(t, api.api, "100.00")

	e.send(t, w, "msg-crash", sqsBet(w, "msg-crash", "bet-crash", "10.00"))
	if code := crashing.waitExit(t, 30*time.Second); code != faultinject.CrashExitCode {
		t.Fatalf("consumer exit code = %d, want %d", code, faultinject.CrashExitCode)
	}
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 9000 {
		t.Fatalf("balance after crash = %d, want 9000 (commit happened)", n)
	}
	if n := e.queueDepth(t, e.queueURL); n != 1 {
		t.Fatalf("queue depth after crash = %d, want the undeleted message", n)
	}

	e.start("recovering-consumer", "consumer", nil)
	eventually(t, "redelivered message to be acknowledged", func() bool { return e.queueDepth(t, e.queueURL) == 0 })

	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 9000 {
		t.Fatalf("balance after redelivery = %d, want 9000", n)
	}
	if n := e.count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Fatalf("debits = %d, want 1", n)
	}
	e.assertFinancialConsistency(t)
}

func TestPublisherCrashBetweenPublishAndConfirmRepublishesSameEventID(t *testing.T) {
	e := newEnv(t)
	api := e.start("api-only", "api", nil)
	w := openWallet(t, api.api, "100.00")
	if r := submit(t, api.api, w, "BET", "bet-outbox", "10.00", ""); r.status != http.StatusOK {
		t.Fatalf("bet: %d %s", r.status, r.raw)
	}
	total := e.count(t, "SELECT count(*) FROM outbox_events")

	crashing := e.launch("crashing-publisher", "outbox", map[string]string{faultinject.EnvCrashPoint: faultinject.PointOutboxAfterPublishBeforeConfirm})
	if code := crashing.waitExit(t, 30*time.Second); code != faultinject.CrashExitCode {
		t.Fatalf("publisher exit code = %d, want %d", code, faultinject.CrashExitCode)
	}
	if n := e.count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL"); n != total {
		t.Fatalf("unpublished events after crash = %d, want %d", n, total)
	}

	e.start("publisher-1", "outbox", nil)
	e.start("publisher-2", "outbox", nil)
	eventually(t, "outbox to be confirmed", func() bool {
		return e.count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") == 0
	})

	events := e.auditEvents(t, total, 20*time.Second)
	if len(events) != total {
		t.Fatalf("distinct audited events = %d, want %d", len(events), total)
	}
	for id, deliveries := range events {
		if deliveries != 1 {
			t.Errorf("event %s delivered %d times, FIFO deduplication by eventId expected", id, deliveries)
		}
	}
}

func TestPendingReferenceSurvivesKillAndIsResolvedByAnotherInstance(t *testing.T) {
	e := newEnv(t)
	first := e.start("node-1", "api", nil)
	w := openWallet(t, first.api, "100.00")

	refund := submit(t, first.api, w, "REFUND", "refund-early", "20.00", "bet-late")
	if refund.status != http.StatusAccepted {
		t.Fatalf("refund: %d %s", refund.status, refund.raw)
	}
	first.kill()

	second := e.start("node-2", "api,pendingref", nil)
	if r := submit(t, second.api, w, "BET", "bet-late", "20.00", ""); r.status != http.StatusOK {
		t.Fatalf("bet: %d %s", r.status, r.raw)
	}
	eventually(t, "refund to be applied", func() bool {
		return e.count(t, "SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'refund-early' AND status = 'PROCESSED'") == 1
	})
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 10000 {
		t.Fatalf("balance = %d, want 10000", n)
	}

	third := e.start("node-3", "api", nil)
	replay := submit(t, third.api, w, "REFUND", "refund-early", "20.00", "bet-late")
	if replay.status != http.StatusOK || replay.body["idempotentReplay"] != true {
		t.Fatalf("replay after resolution: %d %s", replay.status, replay.raw)
	}
	e.assertFinancialConsistency(t)
}

func TestPendingReferenceExpiresWhenReferenceNeverArrives(t *testing.T) {
	e := newEnv(t)
	nodes := []*Instance{
		e.start("node-1", allRoles, map[string]string{"PENDING_TTL": "3s"}),
		e.start("node-2", allRoles, map[string]string{"PENDING_TTL": "3s"}),
	}
	w := openWallet(t, nodes[0].api, "100.00")
	if r := submit(t, nodes[1].api, w, "ROLLBACK", "rollback-orphan", "15.00", "bet-never"); r.status != http.StatusAccepted {
		t.Fatalf("rollback: %d %s", r.status, r.raw)
	}
	eventually(t, "pending reference to expire", func() bool {
		return e.count(t, "SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'rollback-orphan' AND status = 'REJECTED' AND failure_code = 'REFERENCE_NOT_FOUND'") == 1
	})
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 10000 {
		t.Fatalf("balance = %d, want 10000", n)
	}
	e.assertFinancialConsistency(t)
}

func TestKillDuringConcurrentLoadWithClientRetries(t *testing.T) {
	e := newEnv(t)
	nodes := startCluster(e, 3)
	wallets := make([]wallet, 10)
	for i := range wallets {
		wallets[i] = openWallet(t, nodes[0].api, "1000.00")
	}

	var alive atomic.Pointer[[]*Instance]
	survivors := []*Instance{nodes[1], nodes[2]}
	all := nodes
	alive.Store(&all)

	const betsPerWallet = 30
	var completed, interrupted atomic.Int32
	var killer sync.WaitGroup
	killer.Go(func() {
		for completed.Load() < int32(len(wallets)*betsPerWallet/4) {
			time.Sleep(time.Millisecond)
		}
		nodes[0].kill()
		alive.Store(&survivors)
	})

	var wg sync.WaitGroup
	for i, w := range wallets {
		for j := range betsPerWallet {
			wg.Go(func() {
				defer completed.Add(1)
				id := fmt.Sprintf("kill-%d-%d", i, j)
				for attempt := 0; ; attempt++ {
					current := *alive.Load()
					r := submit(t, current[(i+j+attempt)%len(current)].api, w, "BET", id, "7.00", "")
					if r.status == http.StatusOK || r.status == http.StatusUnprocessableEntity {
						return
					}
					interrupted.Add(1)
					if attempt == 20 {
						t.Errorf("bet %s never completed: %d %s", id, r.status, r.raw)
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
			})
		}
	}
	wg.Wait()
	killer.Wait()
	t.Logf("%d requests were interrupted by the crash and retried by clients", interrupted.Load())

	restarted := e.start("node-1-restarted", allRoles, nil)
	for i, w := range wallets {
		if n := e.count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != betsPerWallet {
			t.Errorf("wallet %d debits = %d, want %d", i, n, betsPerWallet)
		}
		r := submit(t, restarted.api, w, "BET", fmt.Sprintf("kill-%d-0", i), "7.00", "")
		if r.status != http.StatusOK || r.body["idempotentReplay"] != true {
			t.Errorf("replay after restart: %d %s", r.status, r.raw)
		}
		if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 100000-betsPerWallet*700 {
			t.Errorf("wallet %d balance = %d, want %d", i, n, 100000-betsPerWallet*700)
		}
	}
	eventually(t, "outbox to drain after the crash", func() bool {
		return e.count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") == 0
	})
	e.assertFinancialConsistency(t)
}

func TestGracefulShutdownCompletesAndRestartPreservesIdempotency(t *testing.T) {
	e := newEnv(t)
	node := e.start("node-1", allRoles, nil)
	w := openWallet(t, node.api, "50.00")
	first := submit(t, node.api, w, "BET", "bet-restart", "5.00", "")
	if first.status != http.StatusOK {
		t.Fatalf("bet: %d %s", first.status, first.raw)
	}
	e.send(t, w, "msg-restart", sqsBet(w, "msg-restart", "bet-restart-sqs", "5.00"))
	eventually(t, "message to be consumed", func() bool { return e.queueDepth(t, e.queueURL) == 0 })
	node.terminate(t)

	again := e.start("node-1-again", allRoles, nil)
	replay := submit(t, again.api, w, "BET", "bet-restart", "5.00", "")
	if replay.status != http.StatusOK || replay.body["idempotentReplay"] != true || replay.body["transactionId"] != first.body["transactionId"] {
		t.Fatalf("replay: %d %s", replay.status, replay.raw)
	}
	conflict := submit(t, again.api, w, "BET", "bet-restart", "6.00", "")
	if conflict.status != http.StatusConflict {
		t.Fatalf("payload change: %d %s", conflict.status, conflict.raw)
	}
	e.send(t, w, "msg-restart-2", sqsBet(w, "msg-restart", "bet-restart-sqs", "5.00"))
	eventually(t, "duplicate message to be consumed", func() bool { return e.queueDepth(t, e.queueURL) == 0 })
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 4000 {
		t.Fatalf("balance = %d, want 4000", n)
	}
	e.assertFinancialConsistency(t)
}

func TestComposeInstancesShareState(t *testing.T) {
	if r := call(t, http.MethodGet, composeAPIURLs[0]+"/health/ready", "", ""); r.status != http.StatusOK {
		t.Skipf("compose application instances are not running: %s", r.raw)
	}
	w := openWallet(t, composeAPIURLs[0], "300.00")
	id := "compose-" + newUUID(t)
	if r := submit(t, composeAPIURLs[1], w, "BET", id, "100.00", ""); r.status != http.StatusOK {
		t.Fatalf("bet: %d %s", r.status, r.raw)
	}
	replay := submit(t, composeAPIURLs[len(composeAPIURLs)-1], w, "BET", id, "100.00", "")
	if replay.status != http.StatusOK || replay.body["idempotentReplay"] != true {
		t.Fatalf("replay: %d %s", replay.status, replay.raw)
	}
	r := call(t, http.MethodPost, composeAPIURLs[1]+"/wallets/"+w.ID+"/reconciliation", mustToken(t, "wallet-internal"), "")
	if r.status != http.StatusOK || r.body["consistent"] != true {
		t.Fatalf("reconciliation: %d %s", r.status, r.raw)
	}
}

func TestDatabaseOutageIsReportedAndRecoveredWithoutDuplicates(t *testing.T) {
	e := newEnv(t)
	database := e.proxyDatabase(t)
	slowRetries := map[string]string{"SQS_RETRY_BASE_DELAY": "2s", "SQS_RETRY_MAX_DELAY": "8s", "SQS_PROCESSING_TIMEOUT": "3s"}
	nodes := []*Instance{e.start("node-1", allRoles, slowRetries), e.start("node-2", allRoles, slowRetries)}
	w := openWallet(t, nodes[0].api, "100.00")

	database.cut()
	eventually(t, "readiness to report the database outage", func() bool {
		return nodes[0].ready(t) == http.StatusServiceUnavailable && nodes[1].ready(t) == http.StatusServiceUnavailable
	})
	during := submit(t, nodes[1].api, w, "BET", "bet-during-outage", "30.00", "")
	if during.status != http.StatusServiceUnavailable || during.header.Get("Retry-After") == "" {
		t.Fatalf("bet during outage: %d %v %s", during.status, during.header, during.raw)
	}
	e.send(t, w, "msg-during-outage", sqsBet(w, "msg-during-outage", "sqs-bet-during-outage", "20.00"))
	time.Sleep(3 * time.Second)

	database.restore()
	eventually(t, "readiness to recover", func() bool {
		return nodes[0].ready(t) == http.StatusOK && nodes[1].ready(t) == http.StatusOK
	})
	retried := submit(t, nodes[0].api, w, "BET", "bet-during-outage", "30.00", "")
	if retried.status != http.StatusOK || retried.body["idempotentReplay"] != false {
		t.Fatalf("client retry after recovery: %d %s", retried.status, retried.raw)
	}
	eventually(t, "message retried after the outage to be consumed", func() bool {
		return e.queueDepth(t, e.queueURL) == 0
	})

	if retries := nodes[0].metric(t, `wallet_sqs_messages_total{queue="wager-transactions-consumer",result="RETRY"}`) +
		nodes[1].metric(t, `wallet_sqs_messages_total{queue="wager-transactions-consumer",result="RETRY"}`); retries == 0 {
		t.Error("the consumer never retried the message during the outage")
	}
	if n := e.queueDepth(t, e.dlqURL); n != 0 {
		t.Fatalf("dlq depth = %d; a temporary outage must not dead-letter messages", n)
	}
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 5000 {
		t.Fatalf("balance = %d, want 5000", n)
	}
	if n := e.count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 2 {
		t.Fatalf("debits = %d, want 2", n)
	}
	eventually(t, "outbox to drain after the outage", func() bool {
		return e.count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") == 0
	})
	e.assertFinancialConsistency(t)
}

func TestBrokerOutageDelaysMessagingWithoutLosingEvents(t *testing.T) {
	e := newEnv(t)
	broker := e.proxyBroker(t)
	node := e.start("node-1", allRoles, map[string]string{"AWS_HEALTH_TIMEOUT": "1s"})
	w := openWallet(t, node.api, "100.00")

	broker.cut()
	eventually(t, "readiness to report the broker outage", func() bool {
		return node.ready(t) == http.StatusServiceUnavailable
	})
	if r := submit(t, node.api, w, "BET", "bet-broker-outage", "10.00", ""); r.status != http.StatusOK {
		t.Fatalf("HTTP must keep working while the broker is down: %d %s", r.status, r.raw)
	}
	e.send(t, w, "msg-broker-outage", sqsBet(w, "msg-broker-outage", "sqs-bet-broker-outage", "15.00"))
	time.Sleep(3 * time.Second)
	if n := e.count(t, "SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'sqs-bet-broker-outage'"); n != 0 {
		t.Fatalf("message consumed while the broker was unreachable")
	}
	pending := e.count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL")
	if pending == 0 {
		t.Fatal("events were published while the broker was unreachable")
	}

	broker.restore()
	eventually(t, "readiness to recover", func() bool { return node.ready(t) == http.StatusOK })
	eventually(t, "message to be consumed after recovery", func() bool { return e.queueDepth(t, e.queueURL) == 0 })
	eventually(t, "outbox to drain after recovery", func() bool {
		return e.count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") == 0
	})

	total := e.count(t, "SELECT count(*) FROM outbox_events")
	events := e.auditEvents(t, total, 20*time.Second)
	if len(events) != total {
		t.Fatalf("audited events = %d, want %d", len(events), total)
	}
	for id, deliveries := range events {
		if deliveries != 1 {
			t.Errorf("event %s delivered %d times", id, deliveries)
		}
	}
	if n := e.count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 7500 {
		t.Fatalf("balance = %d, want 7500", n)
	}
	e.assertFinancialConsistency(t)
}
