// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kafka

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/plugin/kzap"

	"github.com/elastic/apm-data/model"
	apmqueue "github.com/elastic/apm-queue"
	"github.com/elastic/apm-queue/queuecontext"
)

// Encoder encodes a model.APMEvent to a []byte
type Encoder interface {
	// Encode accepts a model.APMEvent and returns the encoded representation.
	Encode(model.APMEvent) ([]byte, error)
}

// RecordMutator mutates the record associated with the model.APMEvent.
// If the RecordMutator returns an error, it is considered fatal.
type RecordMutator func(model.APMEvent, *kgo.Record) error

// ProducerConfig holds configuration for publishing events to Kafka.
type ProducerConfig struct {
	// Brokers holds a slice of (host:port) addresses of the Kafka brokers
	// to which events should be published.
	Brokers []string

	// ClientID to use when connecting to Kafka. This is used for logging
	// and client identification purposes.
	ClientID string
	// Version is the software version to use in the Kafka client. This is
	// useful since it shows up in Kafka metrics and logs.
	Version string

	// Logger is used for logging producer errors.
	Logger *zap.Logger

	// Encoder holds an encoding.Encoder for encoding events.
	Encoder Encoder

	// Sync can be used to indicate whether production should be synchronous.
	Sync bool

	// TopicRouter returns the topic where an event should be produced.
	TopicRouter apmqueue.TopicRouter

	// Mutators holds the list of RecordMutator applied to all the records sent
	// by the producer. If any errors are returned, the producer will not
	// produce and return the error in ProcessBatch.
	Mutators []RecordMutator
	// SASL configures the kgo.Client to use SASL authorization.
	SASL sasl.Mechanism
	// TLS configures the kgo.Client to use TLS for authentication.
	TLS *tls.Config
	// CompressionCodec specifies a list of compression codecs.
	// See kgo.ProducerBatchCompression for more details.
	CompressionCodec []kgo.CompressionCodec
}

// Validate checks that cfg is valid, and returns an error otherwise.
func (cfg ProducerConfig) Validate() error {
	var err []error
	if len(cfg.Brokers) == 0 {
		err = append(err, errors.New("kafka: brokers cannot be empty"))
	}
	if cfg.Logger == nil {
		err = append(err, errors.New("kafka: logger cannot be nil"))
	}
	if cfg.Encoder == nil {
		err = append(err, errors.New("kafka: encoder cannot be nil"))
	}
	if cfg.TopicRouter == nil {
		err = append(err, errors.New("kafka: topic router must be set"))
	}
	return errors.Join(err...)
}

// Producer is a model.BatchProcessor that publishes events to Kafka.
type Producer struct {
	cfg    ProducerConfig
	client *kgo.Client

	mu sync.RWMutex
}

// NewProducer returns a new Producer with the given config.
func NewProducer(cfg ProducerConfig) (*Producer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("kafka: invalid producer config: %w", err)
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.WithLogger(kzap.New(cfg.Logger.Named("kafka"))),
	}
	if cfg.ClientID != "" {
		opts = append(opts, kgo.ClientID(cfg.ClientID))
		if cfg.Version != "" {
			opts = append(opts, kgo.SoftwareNameAndVersion(
				cfg.ClientID, cfg.Version,
			))
		}
	}
	if cfg.TLS != nil {
		opts = append(opts, kgo.DialTLSConfig(cfg.TLS.Clone()))
	}
	if cfg.SASL != nil {
		opts = append(opts, kgo.SASL(cfg.SASL))
	}
	if len(cfg.CompressionCodec) > 0 {
		opts = append(opts, kgo.ProducerBatchCompression(cfg.CompressionCodec...))
	}
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka: failed creating producer: %w", err)
	}
	// Issue a metadata refresh request on construction, so the broker list is
	// populated.
	client.ForceMetadataRefresh()

	return &Producer{
		cfg:    cfg,
		client: client,
	}, nil
}

// Close stops the producer
func (p *Producer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client.Close()
	return nil
}

// ProcessBatch publishes the events in batch to the specified Kafka topic.
func (p *Producer) ProcessBatch(ctx context.Context, batch *model.Batch) error {
	// Take a read lock to prevent Close from closing the client
	// while we're attempting to produce records.
	p.mu.RLock()
	defer p.mu.RUnlock()

	var headers []kgo.RecordHeader
	if m, ok := queuecontext.MetadataFromContext(ctx); ok {
		for k, v := range m {
			headers = append(headers, kgo.RecordHeader{
				Key:   k,
				Value: []byte(v),
			})
		}
	}

	var wg sync.WaitGroup
	wg.Add(len(*batch))
	for _, event := range *batch {
		record := &kgo.Record{
			Headers: headers,
			Topic:   string(p.cfg.TopicRouter(event)),
		}
		for _, rm := range p.cfg.Mutators {
			if err := rm(event, record); err != nil {
				return fmt.Errorf("failed to apply record mutator: %w", err)
			}
		}
		encoded, err := p.cfg.Encoder.Encode(event)
		if err != nil {
			return fmt.Errorf("failed to encode event: %w", err)
		}
		record.Value = encoded
		p.client.Produce(ctx, record, func(msg *kgo.Record, err error) {
			defer wg.Done()
			if err != nil {
				p.cfg.Logger.Error("failed producing message",
					zap.Error(err),
					zap.String("topic", msg.Topic),
					zap.Int64("offset", msg.Offset),
					zap.Int32("partition", msg.Partition),
					zap.Any("headers", headers),
				)
			}
		})
	}
	if p.cfg.Sync {
		wg.Wait()
	}
	return nil
}

// Healthy returns an error if the Kafka client fails to reach a discovered
// broker.
func (p *Producer) Healthy() error {
	if err := p.client.Ping(context.Background()); err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	return nil
}
