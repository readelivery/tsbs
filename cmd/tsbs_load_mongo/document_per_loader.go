package main

import (
	"bufio"
	"log"
	"sync"
	"sync/atomic"

	"bitbucket.org/440-labs/influxdb-comparisons/cmd/tsbs_generate_data/serialize"
	"bitbucket.org/440-labs/influxdb-comparisons/load"
	"github.com/globalsign/mgo"
)

type singleIndexer struct{}

func (i *singleIndexer) GetIndex(_ *serialize.MongoPoint) int { return 0 }

// naiveBenchmark allows you to run a benchmark using the naive, one document per
// event Mongo approach
type naiveBenchmark struct {
	l        *load.BenchmarkRunner
	channels []*duplexChannel
	session  *mgo.Session
}

func newNaiveBenchmark(l *load.BenchmarkRunner, session *mgo.Session) *naiveBenchmark {
	channels := []*duplexChannel{newDuplexChannel(l.NumWorkers())}
	return &naiveBenchmark{l: l, channels: channels, session: session}
}

func (b *naiveBenchmark) Work(wg *sync.WaitGroup, _ int) {
	go processBatchesPerEvent(wg, b.session, b.channels[0])
}

func (b *naiveBenchmark) Scan(batchSize int, br *bufio.Reader) int64 {
	return scanWithIndexer(b.channels, batchSize, br, &singleIndexer{})
}

func (b *naiveBenchmark) Close() {
	b.channels[0].close()
}

type singlePoint struct {
	Measurement string                 `bson:"measurement"`
	Timestamp   int64                  `bson:"timestamp_ns"`
	Fields      map[string]interface{} `bson:"fields"`
	Tags        map[string]string      `bson:"tags"`
}

var spPool = &sync.Pool{New: func() interface{} { return &singlePoint{} }}

// processBatchesPerEvent creates a new document for each incoming event for a simpler
// approach to storing the data. This is _NOT_ the default since the aggregation method
// is recommended by Mongo and other blogs
func processBatchesPerEvent(wg *sync.WaitGroup, session *mgo.Session, dc *duplexChannel) {
	var sess *mgo.Session
	var db *mgo.Database
	var collection *mgo.Collection
	if loader.DoLoad() {
		sess = session.Copy()
		db = sess.DB(loader.DatabaseName())
		collection = db.C(collectionName)
	}
	c := dc.toWorker

	pvs := []interface{}{}
	for batch := range c {
		if cap(pvs) < len(batch) {
			pvs = make([]interface{}, len(batch))
		}
		pvs = pvs[:len(batch)]

		for i, event := range batch {
			x := spPool.Get().(*singlePoint)

			x.Measurement = string(event.MeasurementName())
			x.Timestamp = event.Timestamp()
			x.Fields = map[string]interface{}{}
			x.Tags = map[string]string{}
			f := &serialize.MongoReading{}
			for j := 0; j < event.FieldsLength(); j++ {
				event.Fields(f, j)
				x.Fields[string(f.Key())] = f.Value()
			}
			t := &serialize.MongoTag{}
			for j := 0; j < event.TagsLength(); j++ {
				event.Tags(t, j)
				x.Tags[string(t.Key())] = string(t.Value())
			}
			pvs[i] = x
			atomic.AddUint64(&metricCount, uint64(event.FieldsLength()))
		}

		if loader.DoLoad() {
			bulk := collection.Bulk()
			bulk.Insert(pvs...)
			_, err := bulk.Run()
			if err != nil {
				log.Fatalf("Bulk insert docs err: %s\n", err.Error())
			}
		}
		for _, p := range pvs {
			spPool.Put(p)
		}
		dc.sendToScanner()
	}
	wg.Done()
}
