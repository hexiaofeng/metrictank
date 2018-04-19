package kafka

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	confluent "github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/grafana/metrictank/stats"
	"github.com/raintank/worldping-api/pkg/log"
)

var LogLevel int

type Consumer struct {
	conf             ClientConf
	wg               sync.WaitGroup
	consumer         *confluent.Consumer
	Partitions       []int32
	currentOffsets   map[int32]*int64
	bootTimeOffsets  map[int32]int64
	partitionOffset  map[int32]*stats.Gauge64
	partitionLogSize map[int32]*stats.Gauge64
	partitionLag     map[int32]*stats.Gauge64
	LagMonitor       *LagMonitor
	stopChan         chan struct{}
}

type ClientConf struct {
	ClientID              string
	Broker                string
	Partitions            string
	StartAtOffset         string
	GaugePrefix           string
	Topics                []string
	MessageHandler        func([]byte, int32)
	BatchNumMessages      int
	BufferMax             time.Duration
	ChannelBufferSize     int
	FetchMin              int
	NetMaxOpenRequests    int
	MaxWait               time.Duration
	SessionTimeout        time.Duration
	MetadataRetries       int
	MetadataBackoffTime   time.Duration
	MetadataTimeout       time.Duration
	LagCollectionInterval time.Duration
}

func (c *ClientConf) OffsetIsValid() bool {
	switch c.StartAtOffset {
	case "newest":
	case "oldest":
	default:
		_, err := time.ParseDuration(c.StartAtOffset)
		if err != nil {
			return false
		}
	}
	return true
}

func (c *ClientConf) GetConfluentConfig() *confluent.ConfigMap {
	return GetConfig(c.Broker, c.ClientID, "snappy", c.BatchNumMessages, int(c.BufferMax/time.Millisecond), c.ChannelBufferSize, c.FetchMin, c.NetMaxOpenRequests, int(c.MaxWait/time.Millisecond), int(c.SessionTimeout/time.Millisecond))
}

func NewConfig() *ClientConf {
	return &ClientConf{
		GaugePrefix:           "default.kafka.partition",
		BatchNumMessages:      10000,
		BufferMax:             time.Millisecond * 100,
		ChannelBufferSize:     1000000,
		FetchMin:              1,
		NetMaxOpenRequests:    100,
		MaxWait:               time.Millisecond * 100,
		SessionTimeout:        time.Second * 30,
		MetadataRetries:       5,
		MetadataBackoffTime:   time.Millisecond * 500,
		MetadataTimeout:       time.Second * 10,
		LagCollectionInterval: time.Second * 5,
	}
}

func NewConsumer(conf *ClientConf) (*Consumer, error) {
	if len(conf.Topics) < 1 {
		return nil, fmt.Errorf("kafka-consumer: Requiring at least 1 topic")
	}

	clientConf := conf.GetConfluentConfig()
	clientConf.SetKey("retries", 10)
	clientConf.SetKey("enable.partition.eof", false)
	clientConf.SetKey("enable.auto.offset.store", false)
	clientConf.SetKey("enable.auto.commit", false)
	clientConf.SetKey("go.events.channel.enable", true)
	clientConf.SetKey("go.application.rebalance.enable", true)
	consumer, err := confluent.NewConsumer(clientConf)
	if err != nil {
		return nil, err
	}

	c := Consumer{
		conf:             *conf,
		consumer:         consumer,
		currentOffsets:   make(map[int32]*int64),
		bootTimeOffsets:  make(map[int32]int64),
		partitionOffset:  make(map[int32]*stats.Gauge64),
		partitionLogSize: make(map[int32]*stats.Gauge64),
		partitionLag:     make(map[int32]*stats.Gauge64),
		stopChan:         make(chan struct{}),
	}

	availParts, err := GetPartitions(c.consumer, c.conf.Topics, c.conf.MetadataRetries, int(c.conf.MetadataTimeout/time.Millisecond), c.conf.MetadataBackoffTime)
	if err != nil {
		return nil, err
	}

	log.Info("kafka-consumer: Available partitions %v", availParts)
	if c.conf.Partitions == "*" {
		c.Partitions = availParts
	} else {
		parts := strings.Split(c.conf.Partitions, ",")
		for _, part := range parts {
			i, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("Could not parse partition %q. partitions must be '*' or a comma separated list of id's", part)
			}
			c.Partitions = append(c.Partitions, int32(i))
		}
		missing := DiffPartitions(c.Partitions, availParts)
		if len(missing) > 0 {
			return nil, fmt.Errorf("Configured partitions not in list of available partitions. Missing %v", missing)
		}
	}

	for _, part := range c.Partitions {
		_, offset, err := c.consumer.QueryWatermarkOffsets(c.conf.Topics[0], part, int(c.conf.MetadataTimeout/time.Millisecond))
		if err != nil {
			return nil, fmt.Errorf("Failed to get newest offset for topic %s part %d: %s", c.conf.Topics[0], part, err)
		}
		c.bootTimeOffsets[part] = offset
		c.partitionOffset[part] = stats.NewGauge64(fmt.Sprintf("%s.%d.offset", c.conf.GaugePrefix, part))
		c.partitionLogSize[part] = stats.NewGauge64(fmt.Sprintf("%s.%d.log_size", c.conf.GaugePrefix, part))
		c.partitionLag[part] = stats.NewGauge64(fmt.Sprintf("%s.%d.lag", c.conf.GaugePrefix, part))
	}

	return &c, nil
}

// Creates a lag monitor for the given size
// This needs to be called before Start() or StartAndAwaitBacklog() to prevent
// race conditions between initializing the lag monitor and setting lag values
func (c *Consumer) InitLagMonitor(size int) {
	c.LagMonitor = NewLagMonitor(size, c.Partitions)
}

func (c *Consumer) Start(processBacklog *sync.WaitGroup) error {
	err := c.startConsumer()
	if err != nil {
		return fmt.Errorf("Failed to start consumer: %s", err)
	}

	go c.monitorLag(processBacklog)

	for range c.Partitions {
		go c.consume()
	}

	return nil
}

func (c *Consumer) StartAndAwaitBacklog(backlogProcessTimeout time.Duration) error {
	pre := time.Now()
	processBacklog := new(sync.WaitGroup)
	processBacklog.Add(len(c.Partitions))

	err := c.Start(processBacklog)
	if err != nil {
		return err
	}

	// wait for our backlog to be processed before returning.  This will block metrictank from consuming metrics until
	// we have processed old metricPersist messages. The end result is that we wont overwrite chunks in cassandra that
	// have already been previously written.
	// We don't wait more than backlogProcessTimeout for the backlog to be processed.
	log.Info("kafka-consumer: Waiting for metricPersist backlog to be processed.")
	backlogProcessed := make(chan struct{}, 1)
	go func() {
		processBacklog.Wait()
		backlogProcessed <- struct{}{}
	}()

	select {
	case <-time.After(backlogProcessTimeout):
		log.Warn("kafka-consumer: Processing metricPersist backlog has taken too long, giving up lock after %s.", backlogProcessTimeout)
	case <-backlogProcessed:
		log.Info("kafka-consumer: MetricPersist backlog processed in %s.", time.Since(pre))
	}

	return nil
}

func (c *Consumer) consume() {
	c.wg.Add(1)
	defer c.wg.Done()

	var ok bool
	var offsetPtr *int64
	events := c.consumer.Events()
	for {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case confluent.AssignedPartitions:
				c.consumer.Assign(e.Partitions)
				log.Info("kafka-consumer: Assigned partitions: %+v", e)
			case confluent.RevokedPartitions:
				c.consumer.Unassign()
				log.Info("kafka-consumer: Revoked partitions: %+v", e)
			case *confluent.Message:
				tp := e.TopicPartition
				if LogLevel < 2 {
					log.Debug("kafka-consumer: Received message: Topic %s, Partition: %d, Offset: %d, Key: %x", tp.Topic, tp.Partition, tp.Offset, e.Key)
				}

				if offsetPtr, ok = c.currentOffsets[tp.Partition]; !ok || offsetPtr == nil {
					log.Error(3, "kafka-consumer: Received message of unexpected partition: %s:%d", tp.Topic, tp.Partition)
					continue
				}

				c.conf.MessageHandler(e.Value, tp.Partition)
				atomic.StoreInt64(offsetPtr, int64(tp.Offset))
			case *confluent.Error:
				log.Error(3, "kafka-consumer: Kafka consumer error: %s", e.String())
				return
			}
		case <-c.stopChan:
			log.Info("kafka-consumer: Consumer ended.")
			return
		}
	}
}

func (c *Consumer) monitorLag(processBacklog *sync.WaitGroup) {
	c.wg.Add(1)
	defer c.wg.Done()

	completed := make(map[int32]bool, len(c.Partitions))
	for _, partition := range c.Partitions {
		completed[partition] = false
	}

	storeOffsets := func(ts time.Time) {
		for partition := range c.currentOffsets {
			offset := atomic.LoadInt64(c.currentOffsets[partition])
			c.partitionOffset[partition].Set(int(offset))
			if c.LagMonitor != nil {
				c.LagMonitor.StoreOffset(partition, offset, ts)
			}
			if !completed[partition] && offset >= c.bootTimeOffsets[partition]-1 {
				if processBacklog != nil {
					processBacklog.Done()
				}
				completed[partition] = true
				delete(c.bootTimeOffsets, partition)
				if len(c.bootTimeOffsets) == 0 {
					c.bootTimeOffsets = nil
				}
			}

			_, newest, err := c.consumer.QueryWatermarkOffsets(c.conf.Topics[0], partition, int(c.conf.MetadataTimeout/time.Millisecond))
			if err != nil {
				log.Error(3, "kafka-consumer: Error when querying for offsets: %s", err)
			} else {
				c.partitionLogSize[partition].Set(int(newest))
			}

			if err == nil {
				lag := int(newest - offset)
				c.partitionLag[partition].Set(lag)
				if c.LagMonitor != nil {
					c.LagMonitor.StoreLag(partition, lag)
				}
			}
		}
	}

	ticker := time.NewTicker(c.conf.LagCollectionInterval)
	for {
		select {
		case ts := <-ticker.C:
			storeOffsets(ts)
		case <-c.stopChan:
			storeOffsets(time.Now())
			return
		}
	}
}

func (c *Consumer) startConsumer() error {
	var offset confluent.Offset
	var err error
	var topicPartitions confluent.TopicPartitions
	c.currentOffsets = make(map[int32]*int64, len(c.Partitions))

	for i, topic := range c.conf.Topics {
		for _, partition := range c.Partitions {
			var currentOffset int64
			switch c.conf.StartAtOffset {
			case "oldest":
				currentOffset, err = c.tryGetOffset(topic, partition, int64(confluent.OffsetBeginning), 3, time.Second)
				if err != nil {
					return err
				}
			case "newest":
				currentOffset, err = c.tryGetOffset(topic, partition, int64(confluent.OffsetEnd), 3, time.Second)
				if err != nil {
					return err
				}
			default:
				offsetDuration, err := time.ParseDuration(c.conf.StartAtOffset)
				if err != nil {
					return fmt.Errorf("invalid offest format %s: %s", c.conf.StartAtOffset, err)
				}
				currentOffset = time.Now().Add(-1*offsetDuration).UnixNano() / int64(time.Millisecond)
				currentOffset, err = c.tryGetOffset(topic, partition, currentOffset, 3, time.Second)
				if err != nil {
					log.Warn("kafka-consumer: Failed to get specified offset %s, falling back to \"oldest\"", c.conf.StartAtOffset)
					currentOffset, err = c.tryGetOffset(topic, partition, int64(confluent.OffsetBeginning), 3, time.Second)
					if err != nil {
						return err
					}
				}
			}

			offset, err = confluent.NewOffset(currentOffset)
			if err != nil {
				return err
			}

			topicPartitions = append(topicPartitions, confluent.TopicPartition{
				Topic:     &topic,
				Partition: partition,
				Offset:    offset,
			})

			if i == 0 {
				c.currentOffsets[partition] = &currentOffset
			}
		}
	}

	return c.consumer.Assign(topicPartitions)
}

func (c *Consumer) tryGetOffset(topic string, partition int32, offsetI int64, attempts int, sleep time.Duration) (int64, error) {
	offset, err := confluent.NewOffset(offsetI)
	if err != nil {
		return 0, err
	}

	var beginning, end int64

	attempt := 1
	for {
		if offset == confluent.OffsetBeginning || offset == confluent.OffsetEnd {
			beginning, end, err = c.consumer.QueryWatermarkOffsets(topic, partition, int(c.conf.MetadataTimeout/time.Millisecond))
			if err == nil {
				if offset == confluent.OffsetBeginning {
					return beginning, nil
				} else {
					return end, nil
				}
			}
		} else {
			times := []confluent.TopicPartition{{Topic: &topic, Partition: partition, Offset: offset}}
			times, err = c.consumer.OffsetsForTimes(times, int(c.conf.MetadataTimeout/time.Millisecond))
			if err != nil {
				log.Error(3, "kafka-consumer: Failed to get offset", err)
			} else if len(times) == 0 {
				log.Info("kafka-consumer: Falling back to oldest because no offsets were returned")
				offset = confluent.OffsetBeginning
			} else {
				return int64(times[0].Offset), nil
			}
		}

		if attempt >= attempts {
			break
		}

		log.Warn("kafka-consumer: Error when querying offsets, %d retries left: %s", attempts-attempt, err)
		attempt += 1
		time.Sleep(sleep)
	}

	return 0, fmt.Errorf("Failed to get offset %s of partition %s:%d. %s (attempt %d/%d)", offset.String(), topic, partition, err, attempt, attempts)
}

func (c *Consumer) Stop() {
	close(c.stopChan)
	c.wg.Wait()
	c.consumer.Close()
}
