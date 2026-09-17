package faultinject

const (
	EnvCrashPoint = "FAULT_CRASH_POINT"
	CrashExitCode = 86

	PointConsumerAfterCommitBeforeDelete = "sqs-after-commit-before-delete"
	PointOutboxAfterPublishBeforeConfirm = "outbox-after-publish-before-confirm"
)
