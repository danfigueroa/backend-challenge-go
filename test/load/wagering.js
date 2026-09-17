import http from 'k6/http';
import { check, fail, sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Trend } from 'k6/metrics';

const API_URLS = (__ENV.API_URLS || 'http://app-1:8080,http://app-2:8080,http://app-3:8080').split(',');
const KEYCLOAK_URL = __ENV.KEYCLOAK_URL || 'http://keycloak:8080';
const PROMETHEUS_URL = __ENV.PROMETHEUS_URL || 'http://prometheus:9090';
const LOCALSTACK_URL = __ENV.LOCALSTACK_URL || 'http://localstack:4566';
const AUDIT_QUEUE_URL = __ENV.AUDIT_QUEUE_URL || `${LOCALSTACK_URL}/000000000000/wallet-events-audit.fifo`;
const WALLETS = Number(__ENV.WALLETS || 200);
const HOT_WALLETS = Number(__ENV.HOT_WALLETS || 5);
const RATE = Number(__ENV.RATE || 50);
const HOT_VUS = Number(__ENV.HOT_VUS || 10);
const DURATION = __ENV.DURATION || '2m';
const INITIAL_BALANCE = __ENV.INITIAL_BALANCE || '100000.00';

const processed = new Counter('wallet_processed');
const replays = new Counter('wallet_replays');
const rejections = new Counter('wallet_rejections');
const pendings = new Counter('wallet_pending_references');
const conflicts = new Counter('wallet_idempotency_conflicts');
const unavailable = new Counter('wallet_unavailable');
const unexpected = new Counter('wallet_unexpected_responses');
const divergences = new Counter('wallet_reconciliation_divergences');
const outboxDrain = new Trend('wallet_outbox_drain_seconds');
const operationLatency = new Trend('wallet_operation_duration', true);

export const options = {
  setupTimeout: '5m',
  teardownTimeout: '10m',
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    settlement: {
      executor: 'constant-arrival-rate',
      exec: 'settlement',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 100,
      maxVUs: 400,
    },
    duplicates: {
      executor: 'constant-arrival-rate',
      exec: 'duplicates',
      rate: Math.max(1, Math.floor(RATE / 5)),
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 30,
      maxVUs: 150,
    },
    hotWallets: {
      executor: 'constant-vus',
      exec: 'hotWallet',
      vus: HOT_VUS,
      duration: DURATION,
    },
  },
  thresholds: {
    'http_req_duration{operation:bet}': ['p(95)<250', 'p(99)<500'],
    'http_req_duration{operation:settle}': ['p(95)<250', 'p(99)<500'],
    'http_req_duration{operation:hot_bet}': ['p(95)<500'],
    'http_req_duration{operation:replay}': ['p(95)<150'],
    wallet_unexpected_responses: ['count==0'],
    wallet_reconciliation_divergences: ['count==0'],
    checks: ['rate>0.999'],
  },
};

function token(client) {
  const res = http.post(`${KEYCLOAK_URL}/realms/wagering/protocol/openid-connect/token`, {
    grant_type: 'client_credentials',
    client_id: client,
    client_secret: `${client}-local-secret`,
  }, { tags: { operation: 'token' } });
  if (res.status !== 200) {
    fail(`token for ${client}: ${res.status} ${res.body}`);
  }
  return res.json('access_token');
}

function uuid() {
  const bytes = new Uint8Array(16);
  for (let i = 0; i < 16; i += 1) {
    bytes[i] = Math.floor(Math.random() * 256);
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function api() {
  return API_URLS[Math.floor(Math.random() * API_URLS.length)];
}

function purgeAuditQueue() {
  const res = http.post(LOCALSTACK_URL, {
    Action: 'PurgeQueue',
    QueueUrl: AUDIT_QUEUE_URL,
    Version: '2012-11-05',
  }, {
    headers: { Authorization: 'AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/sqs/aws4_request, SignedHeaders=host, Signature=local' },
    tags: { operation: 'purge' },
  });
  if (res.status !== 200) {
    console.warn(`audit queue purge returned ${res.status}; LocalStack may slow down with a large backlog`);
  }
}

export function setup() {
  purgeAuditQueue();
  const internal = token('wallet-internal');
  const wallets = [];
  for (let i = 0; i < WALLETS + HOT_WALLETS; i += 1) {
    const playerId = uuid();
    const res = http.post(`${API_URLS[i % API_URLS.length]}/wallets`,
      JSON.stringify({ playerId, initialBalance: { amount: INITIAL_BALANCE, currency: 'BRL' } }),
      { headers: { Authorization: `Bearer ${internal}`, 'Content-Type': 'application/json' }, tags: { operation: 'open' } });
    if (res.status !== 201) {
      fail(`open wallet: ${res.status} ${res.body}`);
    }
    wallets.push({ id: res.json('id'), playerId });
  }
  return {
    provider: token('provider-a'),
    wallets: wallets.slice(0, WALLETS),
    hot: wallets.slice(WALLETS),
    startedAt: Date.now(),
  };
}

function submit(data, wallet, kind, externalId, amount, reference, operation) {
  const body = {
    providerId: 'provider-a',
    externalTransactionId: externalId,
    playerId: wallet.playerId,
    walletId: wallet.id,
    roundId: `round-${externalId}`,
    gameId: 'k6-slots',
    kind,
    money: { amount, currency: 'BRL' },
  };
  if (reference) {
    body.referenceExternalTransactionId = reference;
    body.roundId = `round-${reference}`;
  }
  const res = http.post(`${api()}/wagering/transactions`, JSON.stringify(body), {
    headers: {
      Authorization: `Bearer ${data.provider}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': `provider-a:${externalId}`,
    },
    tags: { operation, kind },
  });
  operationLatency.add(res.timings.duration, { operation });
  classify(res);
  return res;
}

function classify(res) {
  switch (res.status) {
    case 200:
      if (res.json('idempotentReplay') === true) {
        replays.add(1);
      } else {
        processed.add(1);
      }
      break;
    case 202:
      pendings.add(1);
      break;
    case 409:
      conflicts.add(1);
      break;
    case 422:
      rejections.add(1);
      break;
    case 503:
      unavailable.add(1);
      break;
    default:
      unexpected.add(1, { status: String(res.status) });
  }
}

function amount(min, max) {
  const cents = Math.floor(min * 100 + Math.random() * (max - min) * 100);
  return `${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, '0')}`;
}

function externalId(prefix) {
  return `${prefix}-${exec.scenario.name}-${exec.vu.idInTest}-${exec.scenario.iterationInTest}-${Date.now()}`;
}

export function settlement(data) {
  const wallet = data.wallets[Math.floor(Math.random() * data.wallets.length)];
  const bet = externalId('bet');
  const stake = amount(1, 50);
  const res = submit(data, wallet, 'BET', bet, stake, null, 'bet');
  check(res, { 'bet processed or rejected': (r) => r.status === 200 || r.status === 422 });
  if (res.status !== 200) {
    return;
  }
  const roll = Math.random();
  if (roll < 0.45) {
    const win = submit(data, wallet, 'WIN', `${bet}-win`, amount(1, 100), bet, 'settle');
    check(win, { 'win settled': (r) => r.status === 200 });
  } else if (roll < 0.9) {
    const loss = submit(data, wallet, 'LOSS', `${bet}-loss`, '0.00', null, 'settle');
    check(loss, { 'loss settled': (r) => r.status === 200 });
  } else {
    const kind = roll < 0.95 ? 'REFUND' : 'ROLLBACK';
    const reversal = submit(data, wallet, kind, `${bet}-${kind.toLowerCase()}`, stake, bet, 'reversal');
    check(reversal, { 'reversal processed': (r) => r.status === 200 });
  }
}

export function duplicates(data) {
  const wallet = data.wallets[Math.floor(Math.random() * data.wallets.length)];
  const bet = externalId('dup');
  const value = amount(1, 20);
  const first = submit(data, wallet, 'BET', bet, value, null, 'bet');
  const responses = http.batch(API_URLS.map((url) => ['POST', `${url}/wagering/transactions`, first.request.body, {
    headers: {
      Authorization: `Bearer ${data.provider}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': `provider-a:${bet}`,
    },
    tags: { operation: 'replay', kind: 'BET' },
  }]));
  for (const res of responses) {
    classify(res);
    check(res, {
      'replay flagged': (r) => r.status === first.status && (r.status !== 200 || r.json('idempotentReplay') === true),
      'replay returns original balance': (r) => r.status !== 200 || r.json('balance.amount') === first.json('balance.amount'),
    });
  }
  const tampered = JSON.parse(first.request.body);
  tampered.money.amount = '99.99';
  const conflict = http.post(`${api()}/wagering/transactions`, JSON.stringify(tampered), {
    headers: {
      Authorization: `Bearer ${data.provider}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': `provider-a:${bet}`,
    },
    tags: { operation: 'conflict', kind: 'BET' },
  });
  classify(conflict);
  check(conflict, { 'payload change rejected with 409': (r) => r.status === 409 });
}

export function hotWallet(data) {
  const wallet = data.hot[exec.vu.idInTest % data.hot.length];
  const res = submit(data, wallet, 'BET', externalId('hot'), amount(1, 5), null, 'hot_bet');
  check(res, { 'hot wallet bet processed or rejected': (r) => r.status === 200 || r.status === 422 });
}

function promQuery(expr) {
  const res = http.get(`${PROMETHEUS_URL}/api/v1/query?query=${encodeURIComponent(expr)}`, { tags: { operation: 'prometheus' } });
  if (res.status !== 200) {
    return null;
  }
  const result = res.json('data.result');
  return result && result.length > 0 ? Number(result[0].value[1]) : 0;
}

export function teardown(data) {
  const internal = token('wallet-internal');
  const drainStarted = Date.now();
  let pending = promQuery('max(wallet_outbox_pending_events)');
  while (pending !== null && pending > 0 && Date.now() - drainStarted < 480000) {
    sleep(1);
    pending = promQuery('max(wallet_outbox_pending_events)');
  }
  outboxDrain.add((Date.now() - drainStarted) / 1000);
  console.log(`outbox drained in ${((Date.now() - drainStarted) / 1000).toFixed(1)}s; peak lag during test: ${promQuery(`max_over_time(max(wallet_outbox_lag_seconds)[${Math.ceil((Date.now() - data.startedAt) / 1000)}s:5s])`)}s`);

  const all = data.wallets.concat(data.hot);
  let checkedEntries = 0;
  for (let i = 0; i < all.length; i += 1) {
    const res = http.post(`${API_URLS[i % API_URLS.length]}/wallets/${all[i].id}/reconciliation`, null, {
      headers: { Authorization: `Bearer ${internal}` },
      tags: { operation: 'reconciliation' },
    });
    if (res.status !== 200 || res.json('consistent') !== true) {
      divergences.add(1);
      console.error(`wallet ${all[i].id} reconciliation: ${res.status} ${res.body}`);
      continue;
    }
    checkedEntries += res.json('checkedEntries');
  }
  console.log(`reconciled ${all.length} wallets and ${checkedEntries} ledger entries`);
}
