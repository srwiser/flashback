package main

import (
	"errors"
	"flag"
	"labix.org/v2/mgo"
	. "replay"
	"runtime"
	"sync/atomic"
	"time"
)

func panicOnError(err error) {
	if err != nil {
		panic(err)
	}
}

var (
	maxOps        int
	numSkipOps    int
	opsFilename   string
	sampleRate    float64
	socketTimeout int64
	startTime     int64
	style         string
	url           string
	verbose       bool
	workers       int
	stderr        string
	stdout        string
	logger        *Logger
)

const (
	// Set one minute timeout on mongo socket connections (nanoseconds) by default
	DEFAULT_MGO_SOCKET_TIMEOUT = 60000000000
)

func init() {
	flag.StringVar(&opsFilename, "ops_filename", "",
		"The file for the serialized ops, generated by the record scripts.")
	flag.StringVar(&url, "url", "",
		"The database server's url, in the format of <host>[:<port>]")
	flag.StringVar(&style, "style", "",
		"How to replay the the ops. You can choose: \n"+
			"	stress: repaly ops at fast as possible\n"+
			"	real: repaly ops in accordance to ops' timestamps")
	flag.IntVar(&workers, "workers", 10,
		"Number of workers that sends ops to database.")
	flag.IntVar(&maxOps, "maxOps", 0,
		"[Optional] Maximal amount of ops to be replayed from the "+
			"ops_filename file. By setting it to `0`, replayer will "+
			"replay all the ops.")
	flag.IntVar(&numSkipOps, "numSkipOps", 0,
		"[Optional] Skip first N ops. Useful for when the total ops in ops_filename"+
			" exceeds available memory and you're running in stress mode.")
	flag.Int64Var(&socketTimeout, "socketTimeout", DEFAULT_MGO_SOCKET_TIMEOUT, "Mongo socket timeout in nanoseconds. Defaults to 60 seconds.")
	flag.Float64Var(&sampleRate, "sample_rate", 0.0, "sample ops for latency")
	// TODO: define a real error logger
	flag.BoolVar(&verbose, "verbose", false, "[Optional] Print op errors and other verbose information to stdout.")
	flag.Int64Var(&startTime, "start_time", 0, "[Optional] Provide a unix timestamp (i.e. 1396456709419)"+
		"indicating the first op that you want to run. Otherwise, play from the top.")

	flag.StringVar(&stderr, "stderr", "", "error/warning log messages will go to stderr")
	flag.StringVar(&stderr, "stdout", "", "regular log messages will go to stderr")
}

func parseFlags() error {
	flag.Parse()
	if style != "stress" && style != "real" {
		return errors.New("Cannot recognize the style: " + style)
	}
	if workers <= 0 {
		return errors.New("`workers` should be a positive number")
	}
	if maxOps == 0 {
		maxOps = 4294967295
	}
	var err error
	if logger, err = NewLogger(stdout, stderr); err != nil {
		return nil
	}
	return nil
}

func RetryOnSocketFailure(block func() error, session *mgo.Session) error {
	err := block()
	if err == nil {
		return nil
	}

	switch err.(type) {
	case *mgo.QueryError:
		return err
	case *mgo.LastError:
		return err
	}

	if err == mgo.ErrNotFound {
		return err
	}

	// Otherwise it's probably a socket error so we refresh the connection,
	// and try again
	session.Refresh()
	logger.Error("retrying mongo query after error: ", err)
	return block()
}

func main() {
	// Will enable system threads to make sure all cpus can be well utilized.
	runtime.GOMAXPROCS(100)
	err := parseFlags()
	panicOnError(err)

	// Prepare to dispatch ops
	var reader OpsReader
	var opsChan chan *Op
	if style == "stress" {
		err, reader = NewFileByLineOpsReader(opsFilename, logger)
		panicOnError(err)
		if startTime > 0 {
			_, err = reader.SetStartTime(startTime)
			panicOnError(err)
		}
		if numSkipOps > 0 {
			err = reader.SkipOps(numSkipOps)
			panicOnError(err)
		}
		opsChan = NewBestEffortOpsDispatcher(reader, maxOps, logger)
	} else {
		// TODO NewCyclicOpsReader: do we really want to make it cyclic?
		reader = NewCyclicOpsReader(func() OpsReader {
			err, reader := NewFileByLineOpsReader(opsFilename, logger)
			panicOnError(err)
			return reader
		}, logger)
		if startTime > 0 {
			_, err = reader.SetStartTime(startTime)
			panicOnError(err)
		}
		if numSkipOps > 0 {
			err = reader.SkipOps(numSkipOps)
			panicOnError(err)
		}
		opsChan = NewByTimeOpsDispatcher(reader, maxOps, logger)
	}

	latencyChan := make(chan Latency, workers)

	// Set up workers to do the job
	exit := make(chan int)
	opsExecuted := int64(0)
	fetch := func(id int, statsCollector IStatsCollector) {
		logger.Infof("Worker #%d report for duty\n", id)

		session, err := mgo.Dial(url)
		panicOnError(err)
		session.SetSocketTimeout(time.Duration(socketTimeout))

		defer session.Close()
		exec := OpsExecutorWithStats(session, statsCollector)
		for {
			op := <-opsChan
			if op == nil {
				break
			}
			block := func() error {
				err := exec.Execute(op)
				return err
			}
			err := RetryOnSocketFailure(block, session)
			if verbose == true && err != nil {
				logger.Error(err)
			}
			atomic.AddInt64(&opsExecuted, 1)
		}
		exit <- 1
		logger.Infof("Worker #%d done!\n", id)
	}
	statsCollectorList := make([]*StatsCollector, workers)
	for i := 0; i < workers; i++ {
		statsCollectorList[i] = NewStatsCollector()
		statsCollectorList[i].SampleLatencies(sampleRate, latencyChan)
		go fetch(i, statsCollectorList[i])
	}

	// Periodically report execution status
	go func() {
		statsAnalyzer := NewStatsAnalyzer(statsCollectorList, &opsExecuted,
			latencyChan, int(sampleRate*float64(maxOps)))
		toFloat := func(nano int64) float64 {
			return float64(nano) / float64(1e6)
		}
		report := func() {
			status := statsAnalyzer.GetStatus()
			logger.Infof("Executed %d ops, %.2f ops/sec", opsExecuted,
				status.OpsPerSec)
			for _, opType := range AllOpTypes {
				allTime := status.AllTimeLatencies[opType]
				sinceLast := status.SinceLastLatencies[opType]
				logger.Infof("  Op type: %s, count: %d, ops/sec: %.2f",
					opType, status.Counts[opType],
					status.TypeOpsSec[opType]*float64(workers))
				template := "   %s: P50: %.2fms, P70: %.2fms, P90: %.2fms, " +
					"P95 %.2fms, P99 %.2fms, Max %.2fms\n"
				logger.Infof(template, "Total", toFloat(allTime[P50]),
					toFloat(allTime[P70]), toFloat(allTime[P90]),
					toFloat(allTime[P95]), toFloat(allTime[P99]),
					toFloat(allTime[P100]))
				logger.Infof(template, "Last ", toFloat(sinceLast[P50]),
					toFloat(sinceLast[P70]), toFloat(sinceLast[P90]),
					toFloat(sinceLast[P95]), toFloat(sinceLast[P99]),
					toFloat(sinceLast[P100]))
			}
		}
		defer report()

		for opsExecuted < int64(maxOps) {
			time.Sleep(5 * time.Second)
			report()
		}
	}()

	// Wait for workers
	received := 0
	for received < workers {
		<-exit
		received += 1
	}
}
