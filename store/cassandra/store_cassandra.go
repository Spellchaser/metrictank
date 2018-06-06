package cassandra

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	schema "gopkg.in/raintank/schema.v1"

	"github.com/gocql/gocql"
	"github.com/grafana/metrictank/cassandra"
	"github.com/grafana/metrictank/mdata"
	"github.com/grafana/metrictank/mdata/chunk"
	"github.com/grafana/metrictank/stats"
	"github.com/grafana/metrictank/tracing"
	"github.com/grafana/metrictank/util"
	"github.com/hailocab/go-hostpool"
	opentracing "github.com/opentracing/opentracing-go"
	tags "github.com/opentracing/opentracing-go/ext"
	"github.com/raintank/worldping-api/pkg/log"
)

// write aggregated data to cassandra.

const Month_sec = 60 * 60 * 24 * 28

const Table_name_format = `metric_%d`

var (
	errChunkTooSmall = errors.New("impossibly small chunk in cassandra")
	errInvalidRange  = errors.New("CassandraStore: invalid range: from must be less than to")
	errReadQueueFull = errors.New("the read queue is full")
	errReadTooOld    = errors.New("the read is too old")
	errTableNotFound = errors.New("table for given TTL not found")
	errCtxCanceled   = errors.New("context canceled")

	// metric store.cassandra.get.exec is the duration of getting from cassandra store
	cassGetExecDuration = stats.NewLatencyHistogram15s32("store.cassandra.get.exec")
	// metric store.cassandra.get.wait is the duration of the get spent in the queue
	cassGetWaitDuration = stats.NewLatencyHistogram12h32("store.cassandra.get.wait")
	// metric store.cassandra.put.exec is the duration of putting in cassandra store
	cassPutExecDuration = stats.NewLatencyHistogram15s32("store.cassandra.put.exec")
	// metric store.cassandra.put.wait is the duration of a put in the wait queue
	cassPutWaitDuration = stats.NewLatencyHistogram12h32("store.cassandra.put.wait")
	// reads that were already too old to be executed
	cassOmitOldRead = stats.NewCounter32("store.cassandra.omit_read.too_old")
	// reads that could not be pushed into the queue because it was full
	cassReadQueueFull = stats.NewCounter32("store.cassandra.omit_read.queue_full")

	// metric store.cassandra.chunks_per_response is how many chunks are retrieved per response in get queries
	cassChunksPerResponse = stats.NewMeter32("store.cassandra.chunks_per_response", false)
	// metric store.cassandra.rows_per_response is how many rows come per get response
	cassRowsPerResponse = stats.NewMeter32("store.cassandra.rows_per_response", false)
	// metric store.cassandra.get_chunks is the duration of how long it takes to get chunks
	cassGetChunksDuration = stats.NewLatencyHistogram15s32("store.cassandra.get_chunks")
	// metric store.cassandra.to_iter is the duration of converting chunks to iterators
	cassToIterDuration = stats.NewLatencyHistogram15s32("store.cassandra.to_iter")

	// metric store.cassandra.chunk_operations.save_ok is counter of successful saves
	chunkSaveOk = stats.NewCounter32("store.cassandra.chunk_operations.save_ok")
	// metric store.cassandra.chunk_operations.save_fail is counter of failed saves
	chunkSaveFail = stats.NewCounter32("store.cassandra.chunk_operations.save_fail")
	// metric store.cassandra.chunk_size.at_save is the sizes of chunks seen when saving them
	chunkSizeAtSave = stats.NewMeter32("store.cassandra.chunk_size.at_save", true)
	// metric store.cassandra.chunk_size.at_load is the sizes of chunks seen when loading them
	chunkSizeAtLoad = stats.NewMeter32("store.cassandra.chunk_size.at_load", true)

	errmetrics = cassandra.NewErrMetrics("store.cassandra")
)

type ChunkReadRequest struct {
	q         string
	p         []interface{}
	timestamp time.Time
	out       chan readResult
	ctx       context.Context
}

type TTLTables map[uint32]ttlTable
type ttlTable struct {
	Table      string
	WindowSize uint32
}

type CassandraStore struct {
	Session          *gocql.Session
	writeQueues      []chan *mdata.ChunkWriteRequest
	writeQueueMeters []*stats.Range32
	readQueue        chan *ChunkReadRequest
	ttlTables        TTLTables
	omitReadTimeout  time.Duration
	tracer           opentracing.Tracer
	timeout          time.Duration
}

func ttlUnits(ttl uint32) float64 {
	// convert ttl to hours
	return float64(ttl) / (60 * 60)
}

func PrepareChunkData(span uint32, data []byte) []byte {
	chunkSizeAtSave.Value(len(data))
	version := chunk.FormatStandardGoTszWithSpan
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, version)

	spanCode, ok := chunk.RevChunkSpans[span]
	if !ok {
		// it's probably better to panic than to persist the chunk with a wrong length
		panic(fmt.Sprintf("Chunk span invalid: %d", span))
	}
	binary.Write(buf, binary.LittleEndian, spanCode)
	buf.Write(data)
	return buf.Bytes()
}

func GetTTLTables(ttls []uint32, windowFactor int, nameFormat string) TTLTables {
	tables := make(TTLTables)
	for _, ttl := range ttls {
		tables[ttl] = GetTTLTable(ttl, windowFactor, nameFormat)
	}
	return tables
}

func GetTTLTable(ttl uint32, windowFactor int, nameFormat string) ttlTable {
	/*
	 * the purpose of this is to bucket metrics of similar TTLs.
	 * we first calculate the largest power of 2 that's smaller than the TTL and then divide the result by
	 * the window factor. for example with a window factor of 20 we want to group the metrics like this:
	 *
	 * generated with: https://gist.github.com/replay/69ad7cfd523edfa552cd12851fa74c58
	 *
	 * +------------------------+---------------+---------------------+----------+
	 * |              TTL hours |    table_name | window_size (hours) | sstables |
	 * +------------------------+---------------+---------------------+----------+
	 * |         0 <= hours < 1 |     metrics_0 |                   1 |    0 - 2 |
	 * |         1 <= hours < 2 |     metrics_1 |                   1 |    1 - 3 |
	 * |         2 <= hours < 4 |     metrics_2 |                   1 |    2 - 5 |
	 * |         4 <= hours < 8 |     metrics_4 |                   1 |    4 - 9 |
	 * |        8 <= hours < 16 |     metrics_8 |                   1 |   8 - 17 |
	 * |       16 <= hours < 32 |    metrics_16 |                   1 |  16 - 33 |
	 * |       32 <= hours < 64 |    metrics_32 |                   2 |  16 - 33 |
	 * |      64 <= hours < 128 |    metrics_64 |                   4 |  16 - 33 |
	 * |     128 <= hours < 256 |   metrics_128 |                   7 |  19 - 38 |
	 * |     256 <= hours < 512 |   metrics_256 |                  13 |  20 - 41 |
	 * |    512 <= hours < 1024 |   metrics_512 |                  26 |  20 - 41 |
	 * |   1024 <= hours < 2048 |  metrics_1024 |                  52 |  20 - 41 |
	 * |   2048 <= hours < 4096 |  metrics_2048 |                 103 |  20 - 41 |
	 * |   4096 <= hours < 8192 |  metrics_4096 |                 205 |  20 - 41 |
	 * |  8192 <= hours < 16384 |  metrics_8192 |                 410 |  20 - 41 |
	 * | 16384 <= hours < 32768 | metrics_16384 |                 820 |  20 - 41 |
	 * | 32768 <= hours < 65536 | metrics_32768 |                1639 |  20 - 41 |
	 * +------------------------+---------------+---------------------+----------+
	 */

	// calculate the pre factor window by finding the largest power of 2 that's smaller than ttl
	preFactorWindow := uint32(math.Exp2(math.Floor(math.Log2(ttlUnits(ttl)))))
	tableName := fmt.Sprintf(nameFormat, preFactorWindow)
	return ttlTable{
		Table:      tableName,
		WindowSize: preFactorWindow/uint32(windowFactor) + 1,
	}
}

func NewCassandraStore(config *StoreConfig, ttls []uint32) (*CassandraStore, error) {
	stats.NewGauge32("store.cassandra.write_queue.size").Set(config.WriteQueueSize)
	stats.NewGauge32("store.cassandra.num_writers").Set(config.WriteConcurrency)

	cluster := gocql.NewCluster(strings.Split(config.Addrs, ",")...)
	if config.SSL {
		cluster.SslOpts = &gocql.SslOptions{
			CaPath:                 config.CaPath,
			EnableHostVerification: config.HostVerification,
		}
	}
	if config.Auth {
		cluster.Authenticator = gocql.PasswordAuthenticator{
			Username: config.Username,
			Password: config.Password,
		}
	}
	cluster.Consistency = gocql.ParseConsistency(config.Consistency)
	cluster.Timeout = time.Duration(config.Timeout) * time.Millisecond
	cluster.ConnectTimeout = cluster.Timeout
	cluster.NumConns = config.WriteConcurrency
	cluster.ProtoVersion = config.CqlProtocolVersion
	cluster.DisableInitialHostLookup = config.DisableInitialHostLookup
	var err error
	tmpSession, err := cluster.CreateSession()
	if err != nil {
		log.Error(3, "cassandra_store: failed to create cassandra session. %s", err.Error())
		return nil, err
	}

	schemaKeyspace := util.ReadEntry(config.SchemaFile, "schema_keyspace").(string)
	schemaTable := util.ReadEntry(config.SchemaFile, "schema_table").(string)

	ttlTables := GetTTLTables(ttls, config.WindowFactor, Table_name_format)

	// create or verify the metrictank keyspace
	if config.CreateKeyspace {
		log.Info("cassandra_store: ensuring that keyspace %s exists.", config.Keyspace)
		err = tmpSession.Query(fmt.Sprintf(schemaKeyspace, config.Keyspace)).Exec()
		if err != nil {
			return nil, err
		}
		for _, result := range ttlTables {
			log.Info("cassandra_store: ensuring that table %s exists.", result.Table)
			err := tmpSession.Query(fmt.Sprintf(schemaTable, config.Keyspace, result.Table, result.WindowSize, result.WindowSize*60*60)).Exec()
			if err != nil {
				return nil, err
			}
		}

		if err != nil {
			return nil, err
		}
	} else {
		var keyspaceMetadata *gocql.KeyspaceMetadata
		// five attempts to verify the keyspace exists before returning an error
	AttemptLoop:
		for attempt := 1; attempt > 0; attempt++ {
			keyspaceMetadata, err = tmpSession.KeyspaceMetadata(config.Keyspace)
			if err != nil {
				log.Warn("cassandra keyspace not found; attempt: %v", attempt)
				if attempt >= 5 {
					return nil, err
				}
				time.Sleep(5 * time.Second)
			} else {
				for _, result := range ttlTables {
					if _, ok := keyspaceMetadata.Tables[result.Table]; !ok {
						log.Warn("cassandra table %s not found; attempt: %v", result.Table, attempt)
						if attempt >= 5 {
							return nil, err
						}
						time.Sleep(5 * time.Second)
						continue AttemptLoop
					}
				}
				break
			}
		}
	}

	tmpSession.Close()
	cluster.Keyspace = config.Keyspace
	cluster.RetryPolicy = &gocql.SimpleRetryPolicy{NumRetries: config.Retries}

	switch config.HostSelectionPolicy {
	case "roundrobin":
		cluster.PoolConfig.HostSelectionPolicy = gocql.RoundRobinHostPolicy()
	case "hostpool-simple":
		cluster.PoolConfig.HostSelectionPolicy = gocql.HostPoolHostPolicy(hostpool.New(nil))
	case "hostpool-epsilon-greedy":
		cluster.PoolConfig.HostSelectionPolicy = gocql.HostPoolHostPolicy(
			hostpool.NewEpsilonGreedy(nil, 0, &hostpool.LinearEpsilonValueCalculator{}),
		)
	case "tokenaware,roundrobin":
		cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(
			gocql.RoundRobinHostPolicy(),
		)
	case "tokenaware,hostpool-simple":
		cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(
			gocql.HostPoolHostPolicy(hostpool.New(nil)),
		)
	case "tokenaware,hostpool-epsilon-greedy":
		cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(
			gocql.HostPoolHostPolicy(
				hostpool.NewEpsilonGreedy(nil, 0, &hostpool.LinearEpsilonValueCalculator{}),
			),
		)
	default:
		return nil, fmt.Errorf("unknown HostSelectionPolicy '%q'", config.HostSelectionPolicy)
	}

	session, err := cluster.CreateSession()
	if err != nil {
		return nil, err
	}
	log.Debug("CS: created session with config %+v", config)
	c := &CassandraStore{
		Session:          session,
		writeQueues:      make([]chan *mdata.ChunkWriteRequest, config.WriteConcurrency),
		writeQueueMeters: make([]*stats.Range32, config.WriteConcurrency),
		readQueue:        make(chan *ChunkReadRequest, config.ReadQueueSize),
		omitReadTimeout:  time.Duration(config.OmitReadTimeout) * time.Second,
		ttlTables:        ttlTables,
		tracer:           opentracing.NoopTracer{},
		timeout:          cluster.Timeout,
	}

	for i := 0; i < config.WriteConcurrency; i++ {
		c.writeQueues[i] = make(chan *mdata.ChunkWriteRequest, config.WriteQueueSize)
		c.writeQueueMeters[i] = stats.NewRange32(fmt.Sprintf("store.cassandra.write_queue.%d.items", i+1))
		go c.processWriteQueue(c.writeQueues[i], c.writeQueueMeters[i])
	}

	for i := 0; i < config.ReadConcurrency; i++ {
		go c.processReadQueue()
	}

	return c, err
}

func (c *CassandraStore) SetTracer(t opentracing.Tracer) {
	c.tracer = t
}

func (c *CassandraStore) Add(cwr *mdata.ChunkWriteRequest) {
	sum := int(cwr.Key.MKey.Org)
	for _, b := range cwr.Key.MKey.Key {
		sum += int(b)
	}
	which := sum % len(c.writeQueues)
	c.writeQueueMeters[which].Value(len(c.writeQueues[which]))
	c.writeQueues[which] <- cwr
}

/* process writeQueue.
 */
func (c *CassandraStore) processWriteQueue(queue chan *mdata.ChunkWriteRequest, meter *stats.Range32) {
	tick := time.Tick(time.Duration(1) * time.Second)
	for {
		select {
		case <-tick:
			meter.Value(len(queue))
		case cwr := <-queue:
			meter.Value(len(queue))
			log.Debug("CS: starting to save %s:%d %v", cwr.Key, cwr.Chunk.T0, cwr.Chunk)
			//log how long the chunk waited in the queue before we attempted to save to cassandra
			cassPutWaitDuration.Value(time.Now().Sub(cwr.Timestamp))

			buf := PrepareChunkData(cwr.Span, cwr.Chunk.Series.Bytes())
			success := false
			attempts := 0
			keyStr := cwr.Key.String()
			for !success {
				err := c.insertChunk(keyStr, cwr.Chunk.T0, cwr.TTL, buf)

				if err == nil {
					success = true
					cwr.Metric.SyncChunkSaveState(cwr.Chunk.T0)
					mdata.SendPersistMessage(keyStr, cwr.Chunk.T0)
					log.Debug("CS: save complete. %s:%d %v", keyStr, cwr.Chunk.T0, cwr.Chunk)
					chunkSaveOk.Inc()
				} else {
					errmetrics.Inc(err)
					if (attempts % 20) == 0 {
						log.Warn("CS: failed to save chunk to cassandra after %d attempts. %v, %s", attempts+1, cwr.Chunk, err)
					}
					chunkSaveFail.Inc()
					sleepTime := 100 * attempts
					if sleepTime > 2000 {
						sleepTime = 2000
					}
					time.Sleep(time.Duration(sleepTime) * time.Millisecond)
					attempts++
				}
			}
		}
	}
}

func (c *CassandraStore) GetTableNames() []string {
	names := make([]string, 0)
	for _, table := range c.ttlTables {
		names = append(names, table.Table)
	}
	return names
}

func (c *CassandraStore) getTable(ttl uint32) (string, error) {
	entry, ok := c.ttlTables[ttl]
	if !ok {
		return "", errTableNotFound
	}
	return entry.Table, nil
}

// Insert Chunks into Cassandra.
//
// key: is the metric_id
// ts: is the start of the aggregated time range.
// data: is the payload as bytes.
func (c *CassandraStore) insertChunk(key string, t0, ttl uint32, data []byte) error {
	// for unit tests
	if c.Session == nil {
		return nil
	}

	table, err := c.getTable(ttl)
	if err != nil {
		return err
	}

	query := fmt.Sprintf("INSERT INTO %s (key, ts, data) values(?,?,?) USING TTL %d", table, ttl)
	row_key := fmt.Sprintf("%s_%d", key, t0/Month_sec) // "month number" based on unix timestamp (rounded down)
	pre := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	ret := c.Session.Query(query, row_key, t0, data).WithContext(ctx).Exec()
	cancel()
	cassPutExecDuration.Value(time.Now().Sub(pre))
	return ret
}

type readResult struct {
	i   *gocql.Iter
	err error
}

func (c *CassandraStore) processReadQueue() {
	for crr := range c.readQueue {
		// check to see if the request has been canceled, if so abort now.
		select {
		case <-crr.ctx.Done():
			//request canceled
			crr.out <- readResult{err: errCtxCanceled}
			continue
		default:
		}
		waitDuration := time.Since(crr.timestamp)
		cassGetWaitDuration.Value(waitDuration)
		if waitDuration > c.omitReadTimeout {
			cassOmitOldRead.Inc()
			crr.out <- readResult{err: errReadTooOld}
			continue
		}

		pre := time.Now()
		iter := readResult{
			i:   c.Session.Query(crr.q, crr.p...).WithContext(crr.ctx).Iter(),
			err: nil,
		}
		cassGetExecDuration.Value(time.Since(pre))
		crr.out <- iter
	}
}

// Basic search of cassandra in the table for given ttl
// start inclusive, end exclusive
func (c *CassandraStore) Search(ctx context.Context, key schema.AMKey, ttl, start, end uint32) ([]chunk.IterGen, error) {
	table, err := c.getTable(ttl)
	if err != nil {
		return nil, err
	}
	return c.SearchTable(ctx, key, table, start, end)
}

// Basic search of cassandra in given table
// start inclusive, end exclusive
func (c *CassandraStore) SearchTable(ctx context.Context, key schema.AMKey, table string, start, end uint32) ([]chunk.IterGen, error) {
	_, span := tracing.NewSpan(ctx, c.tracer, "CassandraStore.SearchTable")
	defer span.Finish()
	tags.SpanKindRPCClient.Set(span)
	tags.PeerService.Set(span, "cassandra")

	var itgens []chunk.IterGen
	if start >= end {
		tracing.Failure(span)
		tracing.Error(span, errInvalidRange)
		return itgens, errInvalidRange
	}

	pre := time.Now()

	start_month := start - (start % Month_sec)       // starting row has to be at, or before, requested start
	end_month := (end - 1) - ((end - 1) % Month_sec) // ending row has to include the last point we might need (end-1)

	// unfortunately in the database we only have the t0's of all chunks.
	// this means we can easily make sure to include the correct last chunk (just query for a t0 < end, the last chunk will contain the last needed data)
	// but it becomes hard to find which should be the first chunk to include. we can't just query for start <= t0 because than will miss some data at the beginning
	// we can't assume we know the chunkSpan so we can't just calculate the t0 >= start - <some-predefined-number> because chunkSpans may change over time.
	// we effectively need all chunks with a t0 > start, as well as the last chunk with a t0 <= start.
	// since we make sure that you can only use chunkSpans so that Month_sec % chunkSpan == 0, we know that this previous chunk will always be in the same row
	// as the one that has start_month.

	// For example:
	// Month_sec = 60 * 60 * 24 * 28 = 2419200 (28 days)
	// Chunkspan = 60 * 60 * 24 * 7  = 604800  (7 days) this is much more than typical chunk size, but this allows us to be more compact in this example
	// row chunk t0      st. end
	// 0   0     2419200
	//     1     3024000
	//     2     3628800
	//     3     4233600
	// 1   4     4838400  /
	//     5     5443200  \
	//     6     6048000
	//     7     6652800
	// 2   8     7257600     /
	//     9     7862400     \
	//     ...   ...

	// let's say query has start 5222000 and end 7555000
	// so start is somewhere between 4-5, and end between 8-9
	// start_month = 4838400 (row 1)
	// end_month = 7257600 (row 2)
	// how do we query for all the chunks we need and not many more? knowing that chunkspan is not known?
	// for end, we can simply search for t0 < 7555000 in row 2, which gives us all chunks we need
	// for start, the best we can do is search for t0 <= 5222000 in row 1
	// note that this may include up to 4 weeks of unneeded data if start falls late within a month.  NOTE: we can set chunkspan "hints" via config

	results := make(chan readResult, 1)

	var rowKeys []string
	for month := start_month; month <= end_month; month += Month_sec {
		rowKeys = append(rowKeys, fmt.Sprintf("%s_%d", key, month/Month_sec))
	}
	// Cannot page queries with both ORDER BY and a IN restriction on the partition key; you must either remove the ORDER BY or the IN and sort client side, or disable paging for this query
	crr := ChunkReadRequest{
		q:         fmt.Sprintf("SELECT ts, data FROM %s WHERE key IN ? AND ts < ?", table),
		p:         []interface{}{rowKeys, end},
		timestamp: pre,
		out:       results,
		ctx:       ctx,
	}

	select {
	case <-ctx.Done():
		// request has been canceled, so no need to continue queuing reads.
		// reads already queued will be aborted when read from the queue.
		return nil, nil
	case c.readQueue <- &crr:
	default:
		cassReadQueueFull.Inc()
		tracing.Failure(span)
		tracing.Error(span, errReadQueueFull)
		return nil, errReadQueueFull
	}

	var res readResult
	select {
	case <-ctx.Done():
		// request has been canceled, so no need to continue processing results
		return nil, nil
	case res = <-results:
		if res.err != nil {
			if res.err == errCtxCanceled {
				// context was canceled, return immediately.
				return nil, nil
			}
			tracing.Failure(span)
			tracing.Error(span, res.err)
			return nil, res.err
		}
		close(results)
	}

	cassGetChunksDuration.Value(time.Since(pre))
	pre = time.Now()

	var b []byte
	var ts int
	for res.i.Scan(&ts, &b) {
		chunkSizeAtLoad.Value(len(b))
		if len(b) < 2 {
			tracing.Failure(span)
			tracing.Error(span, errChunkTooSmall)
			return itgens, errChunkTooSmall
		}
		itgen, err := chunk.NewGen(b, uint32(ts))
		if err != nil {
			tracing.Failure(span)
			tracing.Error(span, err)
			return itgens, err
		}
		itgens = append(itgens, *itgen)
	}

	err := res.i.Close()
	if err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			// query was aborted.
			return nil, nil
		}
		tracing.Failure(span)
		tracing.Error(span, err)
		errmetrics.Inc(err)
		return nil, err
	}

	sort.Sort(chunk.IterGensAsc(itgens))

	cassToIterDuration.Value(time.Now().Sub(pre))
	cassRowsPerResponse.Value(len(rowKeys))
	cassChunksPerResponse.Value(len(itgens))
	span.SetTag("rows", len(rowKeys))
	span.SetTag("chunks", len(itgens))
	return itgens, nil
}

func (c *CassandraStore) Stop() {
	c.Session.Close()
}
