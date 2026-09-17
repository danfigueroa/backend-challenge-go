#!/bin/bash
set -euo pipefail

export REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export ACCOUNT="000000000000"
export INPUT_QUEUE="wager-transactions.fifo"
export DLQ="wager-transactions-dlq.fifo"
export TOPIC="wallet-events.fifo"
export AUDIT_QUEUE="wallet-events-audit.fifo"
export MAX_RECEIVE_COUNT="${WAGER_QUEUE_MAX_RECEIVE_COUNT:-5}"
export VISIBILITY_TIMEOUT="${WAGER_QUEUE_VISIBILITY_TIMEOUT:-30}"
POLICIES="/etc/localstack/policies"

queue_arn() { echo "arn:aws:sqs:${REGION}:${ACCOUNT}:$1"; }
queue_url() { awslocal sqs get-queue-url --queue-name "$1" --query QueueUrl --output text; }
json() { python3 -c "import json, os, sys; print(json.dumps(eval(sys.argv[1], {'env': os.environ, 'json': json})))" "$1"; }

echo "provisioning dead-letter queue ${DLQ}"
awslocal sqs create-queue --queue-name "${DLQ}" --attributes "$(json "{
  'FifoQueue': 'true', 'ContentBasedDeduplication': 'false', 'MessageRetentionPeriod': '1209600'}")" >/dev/null

echo "provisioning input queue ${INPUT_QUEUE} (maxReceiveCount=${MAX_RECEIVE_COUNT}, visibility=${VISIBILITY_TIMEOUT}s)"
awslocal sqs create-queue --queue-name "${INPUT_QUEUE}" --attributes "$(json "{
  'FifoQueue': 'true', 'ContentBasedDeduplication': 'false',
  'VisibilityTimeout': env['VISIBILITY_TIMEOUT'], 'ReceiveMessageWaitTimeSeconds': '20', 'MessageRetentionPeriod': '345600',
  'RedrivePolicy': json.dumps({'deadLetterTargetArn': 'arn:aws:sqs:%s:%s:%s' % (env['REGION'], env['ACCOUNT'], env['DLQ']), 'maxReceiveCount': env['MAX_RECEIVE_COUNT']})}")" >/dev/null
awslocal sqs set-queue-attributes --queue-url "$(queue_url "${DLQ}")" --attributes "$(json "{
  'RedriveAllowPolicy': json.dumps({'redrivePermission': 'byQueue', 'sourceQueueArns': ['arn:aws:sqs:%s:%s:%s' % (env['REGION'], env['ACCOUNT'], env['INPUT_QUEUE'])]})}")"

echo "provisioning events topic ${TOPIC} and audit subscription ${AUDIT_QUEUE}"
export TOPIC_ARN=$(awslocal sns create-topic --name "${TOPIC}" --attributes FifoTopic=true,ContentBasedDeduplication=false --query TopicArn --output text)
awslocal sqs create-queue --queue-name "${AUDIT_QUEUE}" --attributes "$(json "{
  'FifoQueue': 'true', 'ContentBasedDeduplication': 'false', 'MessageRetentionPeriod': '1209600'}")" >/dev/null
awslocal sqs set-queue-attributes --queue-url "$(queue_url "${AUDIT_QUEUE}")" --attributes "$(json "{
  'Policy': json.dumps({'Version': '2012-10-17', 'Statement': [{
    'Sid': 'AllowWalletEventsTopic', 'Effect': 'Allow', 'Principal': {'Service': 'sns.amazonaws.com'},
    'Action': 'sqs:SendMessage', 'Resource': 'arn:aws:sqs:%s:%s:%s' % (env['REGION'], env['ACCOUNT'], env['AUDIT_QUEUE']),
    'Condition': {'ArnEquals': {'aws:SourceArn': env['TOPIC_ARN']}}}]})}")"
awslocal sns subscribe --topic-arn "${TOPIC_ARN}" --protocol sqs --notification-endpoint "$(queue_arn "${AUDIT_QUEUE}")" \
  --attributes RawMessageDelivery=true >/dev/null

echo "provisioning least-privilege identities"
for identity in wager-transactions-producer wager-transactions-consumer wallet-events-publisher; do
  awslocal iam get-user --user-name "${identity}" >/dev/null 2>&1 || awslocal iam create-user --user-name "${identity}" >/dev/null
  awslocal iam get-policy --policy-arn "arn:aws:iam::${ACCOUNT}:policy/${identity}" >/dev/null 2>&1 \
    || awslocal iam create-policy --policy-name "${identity}" --policy-document "file://${POLICIES}/${identity}.json" >/dev/null
  awslocal iam attach-user-policy --user-name "${identity}" --policy-arn "arn:aws:iam::${ACCOUNT}:policy/${identity}"
done

awslocal sqs set-queue-attributes --queue-url "$(queue_url "${INPUT_QUEUE}")" --attributes "$(json "{
  'Policy': json.dumps({'Version': '2012-10-17', 'Statement': [
    {'Sid': 'ProducersSend', 'Effect': 'Allow', 'Principal': {'AWS': 'arn:aws:iam::%s:user/wager-transactions-producer' % env['ACCOUNT']},
     'Action': 'sqs:SendMessage', 'Resource': 'arn:aws:sqs:%s:%s:%s' % (env['REGION'], env['ACCOUNT'], env['INPUT_QUEUE'])},
    {'Sid': 'ConsumerReceives', 'Effect': 'Allow', 'Principal': {'AWS': 'arn:aws:iam::%s:user/wager-transactions-consumer' % env['ACCOUNT']},
     'Action': ['sqs:ReceiveMessage', 'sqs:DeleteMessage', 'sqs:ChangeMessageVisibility', 'sqs:GetQueueAttributes'],
     'Resource': 'arn:aws:sqs:%s:%s:%s' % (env['REGION'], env['ACCOUNT'], env['INPUT_QUEUE'])}]})}")"

echo "messaging provisioned"
