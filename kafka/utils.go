package kafka

import (
	confluent "github.com/confluentinc/confluent-kafka-go/kafka"
)

func GetConfig(broker, client, compression string, batchNumMessages, bufferMaxMs, bufferSize, fetchMin, maxOpenRequests, maxWaitMs, sessionTimeoutMs int) *confluent.ConfigMap {
	return &confluent.ConfigMap{
		"bootstrap.servers":                     broker,
		"compression.codec":                     "snappy",
		"group.id":                              client,
		"fetch.min.bytes":                       fetchMin,
		"fetch.wait.max.ms":                     maxWaitMs,
		"max.in.flight.requests.per.connection": maxOpenRequests,
		"queue.buffering.max.messages":          bufferSize,
		"retries":                               10,
		"session.timeout.ms":                    sessionTimeoutMs,
		"queue.buffering.max.ms":                bufferMaxMs,
		"batch.num.messages":                    batchNumMessages,
	}
}
