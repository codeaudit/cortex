package chunk

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/storage/metric"
	"github.com/sburnett/lexicographic-tuples"
	"github.com/weaveworks/scope/common/instrument"
	"golang.org/x/net/context"

	"github.com/weaveworks/cortex/user"
)

const (
	hashKey  = "h"
	rangeKey = "r"
	chunkKey = "c"

	secondsInHour = int64(time.Hour / time.Second)
	secondsInDay  = int64(24 * time.Hour / time.Second)
)

var (
	indexEntriesPerChunk = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "chunk_store_index_entries_per_chunk",
		Help:      "Number of entries written to dynamodb per chunk.",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 5),
	})
	s3RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "s3_request_duration_seconds",
		Help:      "Time spent doing S3 requests.",
		Buckets:   []float64{.025, .05, .1, .25, .5, 1, 2},
	}, []string{"operation", "status_code"})

	queryChunks = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "query_chunks",
		Help:      "Number of chunks loaded per query.",
		Buckets:   prometheus.ExponentialBuckets(1, 4.0, 5),
	})
	queryDynamoLookups = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "query_dynamo_lookups",
		Help:      "Number of dynamo lookups per query.",
		Buckets:   prometheus.ExponentialBuckets(1, 4.0, 5),
	})
	queryRequestPages = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "query_dynamo_request_pages",
		Help:      "Number of pages per DynamoDB request",
		Buckets:   prometheus.ExponentialBuckets(1, 2.0, 5),
	})
	queryDroppedMatches = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "query_dynamo_dropped_matches_total",
		Help:      "The number of chunks IDs fetched from Dynamo but later dropped for not matching (per DynamoDB request).",
		Buckets:   prometheus.ExponentialBuckets(1, 2.0, 5),
	})
)

func init() {
	prometheus.MustRegister(indexEntriesPerChunk)
	prometheus.MustRegister(s3RequestDuration)
	prometheus.MustRegister(queryChunks)
	prometheus.MustRegister(queryDynamoLookups)
	prometheus.MustRegister(queryRequestPages)
	prometheus.MustRegister(queryDroppedMatches)
}

// Store type stores and indexes chunks
type Store interface {
	Put(ctx context.Context, chunks []Chunk) error
	Get(ctx context.Context, from, through model.Time, matchers ...*metric.LabelMatcher) ([]Chunk, error)
}

// StoreConfig specifies config for a ChunkStore
type StoreConfig struct {
	S3         S3Client
	BucketName string
	DynamoDB   DynamoDBClient
	TableName  string
	ChunkCache *Cache

	// After midnight on this day, we start bucketing indexes by day instead of by
	// hour.  Only the day matters, not the time within the day.
	DailyBucketsFrom model.Time

	PeriodicTableConfig
}

// PeriodicTableConfig for the use of periodic tables (ie, weekly talbes).  Can
// control when to start the periodic tables, how long the period should be,
// and the prefix to give the tables.
type PeriodicTableConfig struct {
	UsePeriodicTables    bool
	TablePrefix          string
	TablePeriod          time.Duration
	PeriodicTableStartAt time.Time
}

// AWSStore implements ChunkStore for AWS
type AWSStore struct {
	cfg StoreConfig

	dynamo *dynamoDBBackoffClient
}

// NewAWSStore makes a new ChunkStore
func NewAWSStore(cfg StoreConfig) *AWSStore {
	return &AWSStore{
		cfg:    cfg,
		dynamo: newDynamoDBBackoffClient(cfg.DynamoDB),
	}
}

type bucketSpec struct {
	tableName string
	bucket    string
}

// bigBuckets generates the list of "big buckets" for a given time range.
// These buckets are used in the hash key of the inverted index, and need to
// be deterministic for both reads and writes.
//
// This function deals with any changes from one bucketing scheme to another -
// for instance, it knows the date at which to migrate from hourly buckets to
// to weekly buckets.
func (c *AWSStore) bigBuckets(from, through model.Time) []bucketSpec {
	var (
		fromHour    = from.Unix() / secondsInHour
		throughHour = through.Unix() / secondsInHour

		fromDay    = from.Unix() / secondsInDay
		throughDay = through.Unix() / secondsInDay

		firstDailyBucket = c.cfg.DailyBucketsFrom.Unix() / secondsInDay
		lastHourlyBucket = firstDailyBucket * 24

		result []bucketSpec
	)

	for i := fromHour; i <= throughHour; i++ {
		if i >= lastHourlyBucket {
			break
		}
		result = append(result, bucketSpec{
			tableName: c.tableForBucket(i * secondsInHour),
			bucket:    strconv.Itoa(int(i)),
		})
	}

	for i := fromDay; i <= throughDay; i++ {
		if i < firstDailyBucket {
			continue
		}
		result = append(result, bucketSpec{
			tableName: c.tableForBucket(i * secondsInDay),
			bucket:    fmt.Sprintf("d%d", int(i)),
		})
	}

	return result
}

func (c *AWSStore) tableForBucket(bucketStart int64) string {
	if !c.cfg.UsePeriodicTables || bucketStart < (c.cfg.PeriodicTableStartAt.Unix()) {
		return c.cfg.TableName
	}
	return c.cfg.TablePrefix + strconv.Itoa(int(bucketStart/int64(c.cfg.TablePeriod/time.Second)))
}

func chunkName(userID, chunkID string) string {
	return fmt.Sprintf("%s/%s", userID, chunkID)
}

func hashValue(userID, bucket string, metricName model.LabelValue) string {
	return fmt.Sprintf("%s:%s:%s", userID, bucket, metricName)
}

func rangeValue(label model.LabelName, value model.LabelValue, chunkID string) ([]byte, error) {
	return lex.Encode(string(label), string(value), chunkID)
}

func parseRangeValue(v []byte) (label model.LabelName, value model.LabelValue, chunkID string, err error) {
	var labelStr, valueStr string
	_, err = lex.Decode(v, &labelStr, &valueStr, &chunkID)
	label, value = model.LabelName(labelStr), model.LabelValue(valueStr)
	return
}

// Put implements ChunkStore
func (c *AWSStore) Put(ctx context.Context, chunks []Chunk) error {
	userID, err := user.GetID(ctx)
	if err != nil {
		return err
	}

	err = c.putChunks(ctx, userID, chunks)
	if err != nil {
		return err
	}

	return c.updateIndex(ctx, userID, chunks)
}

// putChunks writes a collection of chunks to S3 in parallel.
func (c *AWSStore) putChunks(ctx context.Context, userID string, chunks []Chunk) error {
	incomingErrors := make(chan error)
	for _, chunk := range chunks {
		go func(chunk Chunk) {
			incomingErrors <- c.putChunk(ctx, userID, &chunk)
		}(chunk)
	}

	var lastErr error
	for range chunks {
		err := <-incomingErrors
		if err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// putChunk puts a chunk into S3.
func (c *AWSStore) putChunk(ctx context.Context, userID string, chunk *Chunk) error {
	body, err := chunk.reader()
	if err != nil {
		return err
	}

	err = instrument.TimeRequestHistogram(ctx, "S3.PutObject", s3RequestDuration, func(_ context.Context) error {
		var err error
		_, err = c.cfg.S3.PutObject(&s3.PutObjectInput{
			Body:   body,
			Bucket: aws.String(c.cfg.BucketName),
			Key:    aws.String(chunkName(userID, chunk.ID)),
		})
		return err
	})
	if err != nil {
		return err
	}

	if c.cfg.ChunkCache != nil {
		if err = c.cfg.ChunkCache.StoreChunkData(ctx, userID, chunk); err != nil {
			log.Warnf("Could not store %v in chunk cache: %v", chunk.ID, err)
		}
	}
	return nil
}

func (c *AWSStore) updateIndex(ctx context.Context, userID string, chunks []Chunk) error {
	writeReqs, err := c.calculateDynamoWrites(userID, chunks)
	if err != nil {
		return err
	}

	return c.dynamo.batchWriteDynamo(ctx, writeReqs)
}

// calculateDynamoWrites creates a set of batched WriteRequests to dynamo for all
// the chunks it is given.
//
// Creates one WriteRequest per bucket per metric per chunk.
func (c *AWSStore) calculateDynamoWrites(userID string, chunks []Chunk) (map[string][]*dynamodb.WriteRequest, error) {
	writeReqs := map[string][]*dynamodb.WriteRequest{}
	for _, chunk := range chunks {
		metricName, ok := chunk.Metric[model.MetricNameLabel]
		if !ok {
			return nil, fmt.Errorf("no MetricNameLabel for chunk")
		}

		entries := 0
		for _, bucket := range c.bigBuckets(chunk.From, chunk.Through) {
			hashValue := hashValue(userID, bucket.bucket, metricName)
			for label, value := range chunk.Metric {
				if label == model.MetricNameLabel {
					continue
				}

				entries++
				rangeValue, err := rangeValue(label, value, chunk.ID)
				if err != nil {
					return nil, err
				}
				writeReqs[bucket.tableName] = append(writeReqs[bucket.tableName], &dynamodb.WriteRequest{
					PutRequest: &dynamodb.PutRequest{
						Item: map[string]*dynamodb.AttributeValue{
							hashKey:  {S: aws.String(hashValue)},
							rangeKey: {B: rangeValue},
						},
					},
				})
			}
		}
		indexEntriesPerChunk.Observe(float64(entries))
	}
	return writeReqs, nil
}

// Get implements ChunkStore
func (c *AWSStore) Get(ctx context.Context, from, through model.Time, matchers ...*metric.LabelMatcher) ([]Chunk, error) {
	userID, err := user.GetID(ctx)
	if err != nil {
		return nil, err
	}

	missing, err := c.lookupChunks(ctx, userID, from, through, matchers)
	if err != nil {
		return nil, err
	}
	queryChunks.Observe(float64(len(missing)))

	var fromCache []Chunk
	if c.cfg.ChunkCache != nil {
		fromCache, missing, err = c.cfg.ChunkCache.FetchChunkData(ctx, userID, missing)
		if err != nil {
			log.Warnf("Error fetching from cache: %v", err)
		}
	}

	fromS3, err := c.fetchChunkData(ctx, userID, missing)
	if err != nil {
		return nil, err
	}

	if c.cfg.ChunkCache != nil {
		if err = c.cfg.ChunkCache.StoreChunks(ctx, userID, fromS3); err != nil {
			log.Warnf("Could not store chunks in chunk cache: %v", err)
		}
	}

	// TODO instead of doing this sort, propagate an index and assign chunks
	// into the result based on that index.
	allChunks := append(fromCache, fromS3...)
	sort.Sort(ByID(allChunks))
	return allChunks, nil
}

func extractMetricName(matchers []*metric.LabelMatcher) (model.LabelValue, []*metric.LabelMatcher, error) {
	for i, matcher := range matchers {
		if matcher.Name != model.MetricNameLabel {
			continue
		}
		if matcher.Type != metric.Equal {
			return "", nil, fmt.Errorf("must have equality matcher for MetricNameLabel")
		}
		metricName := matcher.Value
		matchers = matchers[:i+copy(matchers[i:], matchers[i+1:])]
		return metricName, matchers, nil
	}
	return "", nil, fmt.Errorf("no matcher for MetricNameLabel")
}

func (c *AWSStore) lookupChunks(ctx context.Context, userID string, from, through model.Time, matchers []*metric.LabelMatcher) ([]Chunk, error) {
	metricName, matchers, err := extractMetricName(matchers)
	if err != nil {
		return nil, err
	}

	incomingChunkSets := make(chan ByID)
	incomingErrors := make(chan error)
	buckets := c.bigBuckets(from, through)
	totalLookups := int32(0)
	for _, b := range buckets {
		go func(bucket bucketSpec) {
			incoming, lookups, err := c.lookupChunksFor(ctx, userID, bucket, metricName, matchers)
			atomic.AddInt32(&totalLookups, lookups)
			if err != nil {
				incomingErrors <- err
			} else {
				incomingChunkSets <- incoming
			}
		}(b)
	}

	var chunks ByID
	var lastErr error
	for i := 0; i < len(buckets); i++ {
		select {
		case incoming := <-incomingChunkSets:
			chunks = merge(chunks, incoming)
		case err := <-incomingErrors:
			lastErr = err
		}
	}

	// Filter out chunks that are not in the selected time range.
	filtered := make([]Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		_, chunkFrom, chunkThrough, err := parseChunkID(chunk.ID)
		if err != nil {
			return nil, err
		}
		if chunkThrough < from || through < chunkFrom {
			continue
		}
		filtered = append(filtered, chunk)
	}

	queryDynamoLookups.Observe(float64(atomic.LoadInt32(&totalLookups)))
	return filtered, lastErr
}

func (c *AWSStore) lookupChunksFor(ctx context.Context, userID string, bucket bucketSpec, metricName model.LabelValue, matchers []*metric.LabelMatcher) (ByID, int32, error) {
	if len(matchers) == 0 {
		return c.lookupChunksForMetricName(ctx, userID, bucket, metricName)
	}

	incomingChunkSets := make(chan ByID)
	incomingErrors := make(chan error)

	for _, matcher := range matchers {
		go func(matcher *metric.LabelMatcher) {
			incoming, err := c.lookupChunksForMatcher(ctx, userID, bucket, metricName, matcher)
			if err != nil {
				incomingErrors <- err
			} else {
				incomingChunkSets <- incoming
			}
		}(matcher)
	}

	var chunkSets []ByID
	var lastErr error
	for i := 0; i < len(matchers); i++ {
		select {
		case incoming := <-incomingChunkSets:
			chunkSets = append(chunkSets, incoming)
		case err := <-incomingErrors:
			lastErr = err
		}
	}
	return nWayIntersect(chunkSets), int32(len(matchers)), lastErr
}

func (c *AWSStore) lookupChunksForMetricName(ctx context.Context, userID string, bucket bucketSpec, metricName model.LabelValue) (ByID, int32, error) {
	hashValue := hashValue(userID, bucket.bucket, metricName)
	input := &dynamodb.QueryInput{
		TableName: aws.String(bucket.tableName),
		KeyConditions: map[string]*dynamodb.Condition{
			hashKey: {
				AttributeValueList: []*dynamodb.AttributeValue{
					{S: aws.String(hashValue)},
				},
				ComparisonOperator: aws.String("EQ"),
			},
		},
		ReturnConsumedCapacity: aws.String(dynamodb.ReturnConsumedCapacityTotal),
	}

	chunkSet := ByID{}
	var processingError error
	var pages, totalDropped int
	defer func() {
		queryRequestPages.Observe(float64(pages))
		queryDroppedMatches.Observe(float64(totalDropped))
	}()

	if err := c.dynamo.queryPages(ctx, input, func(resp interface{}, lastPage bool) (shouldContinue bool) {
		var dropped int
		dropped, processingError = processResponse(resp.(*dynamodb.QueryOutput), &chunkSet, nil)
		totalDropped += dropped
		pages++
		return processingError != nil && !lastPage
	}); err != nil {
		log.Errorf("Error querying DynamoDB: %v", err)
		return nil, 1, err
	} else if processingError != nil {
		log.Errorf("Error processing DynamoDB response: %v", processingError)
		return nil, 1, processingError
	}
	sort.Sort(ByID(chunkSet))
	chunkSet = unique(chunkSet)
	return chunkSet, 1, nil
}

func (c *AWSStore) lookupChunksForMatcher(ctx context.Context, userID string, bucket bucketSpec, metricName model.LabelValue, matcher *metric.LabelMatcher) (ByID, error) {
	hashValue := hashValue(userID, bucket.bucket, metricName)
	var rangePrefix []byte
	var err error
	if matcher.Type == metric.Equal {
		rangePrefix, err = lex.Encode(string(matcher.Name), string(matcher.Value))
	} else {
		rangePrefix, err = lex.Encode(string(matcher.Name))
	}
	if err != nil {
		return nil, err
	}

	input := &dynamodb.QueryInput{
		TableName: aws.String(bucket.tableName),
		KeyConditions: map[string]*dynamodb.Condition{
			hashKey: {
				AttributeValueList: []*dynamodb.AttributeValue{
					{S: aws.String(hashValue)},
				},
				ComparisonOperator: aws.String("EQ"),
			},
			rangeKey: {
				AttributeValueList: []*dynamodb.AttributeValue{
					{B: rangePrefix},
				},
				ComparisonOperator: aws.String(dynamodb.ComparisonOperatorBeginsWith),
			},
		},
		ReturnConsumedCapacity: aws.String(dynamodb.ReturnConsumedCapacityTotal),
	}

	chunkSet := ByID{}
	var processingError error
	var pages, totalDropped int
	defer func() {
		queryRequestPages.Observe(float64(pages))
		queryDroppedMatches.Observe(float64(totalDropped))
	}()
	if err := c.dynamo.queryPages(ctx, input, func(resp interface{}, lastPage bool) (shouldContinue bool) {
		var dropped int
		dropped, processingError = processResponse(resp.(*dynamodb.QueryOutput), &chunkSet, matcher)
		totalDropped += dropped
		pages++
		return processingError != nil && !lastPage
	}); err != nil {
		log.Errorf("Error querying DynamoDB: %v", err)
		return nil, err
	} else if processingError != nil {
		log.Errorf("Error processing DynamoDB response: %v", processingError)
		return nil, processingError
	}

	sort.Sort(ByID(chunkSet))
	return chunkSet, nil
}

func processResponse(resp *dynamodb.QueryOutput, chunkSet *ByID, matcher *metric.LabelMatcher) (int, error) {
	dropped := 0
	for _, item := range resp.Items {
		rangeValue := item[rangeKey].B
		if rangeValue == nil {
			return dropped, fmt.Errorf("invalid item: %v", item)
		}
		label, value, chunkID, err := parseRangeValue(rangeValue)
		if err != nil {
			return dropped, err
		}

		chunk := Chunk{
			ID: chunkID,
		}

		if chunkValue, ok := item[chunkKey]; ok && chunkValue.B != nil {
			if err := json.Unmarshal(chunkValue.B, &chunk); err != nil {
				return dropped, err
			}
			chunk.metadataInIndex = true
		}

		if matcher != nil && (label != matcher.Name || !matcher.Match(value)) {
			log.Debugf("Dropping unexpected", chunk.Metric)
			dropped++
			continue
		}
		*chunkSet = append(*chunkSet, chunk)
	}
	return dropped, nil
}

func (c *AWSStore) fetchChunkData(ctx context.Context, userID string, chunkSet []Chunk) ([]Chunk, error) {
	incomingChunks := make(chan Chunk)
	incomingErrors := make(chan error)
	for _, chunk := range chunkSet {
		go func(chunk Chunk) {
			var resp *s3.GetObjectOutput
			err := instrument.TimeRequestHistogram(ctx, "S3.GetObject", s3RequestDuration, func(_ context.Context) error {
				var err error
				resp, err = c.cfg.S3.GetObject(&s3.GetObjectInput{
					Bucket: aws.String(c.cfg.BucketName),
					Key:    aws.String(chunkName(userID, chunk.ID)),
				})
				return err
			})
			if err != nil {
				incomingErrors <- err
				return
			}
			defer resp.Body.Close()
			if err := chunk.decode(resp.Body); err != nil {
				incomingErrors <- err
				return
			}
			incomingChunks <- chunk
		}(chunk)
	}

	chunks := []Chunk{}
	errors := []error{}
	for i := 0; i < len(chunkSet); i++ {
		select {
		case chunk := <-incomingChunks:
			chunks = append(chunks, chunk)
		case err := <-incomingErrors:
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		return nil, errors[0]
	}
	return chunks, nil
}
