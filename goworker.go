package goworker

import (
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/context"

	"errors"
	"github.com/cihub/seelog"
	"github.com/youtube/vitess/go/pools"
	"net"
)

var (
	logger         seelog.LoggerInterface
	pool           *pools.ResourcePool
	ctx            context.Context
	initMutex      sync.Mutex
	initialized    bool
	workerSettings WorkerSettings
)

type WorkerSettings struct {
	QueuesString   string
	Queues         queuesFlag
	IntervalFloat  float64
	Interval       intervalFlag
	Concurrency    int
	Connections    int
	URI            string
	Namespace      string
	ExitOnComplete bool
	IsStrict       bool
	UseNumber      bool
	RedisSettings  RedisSettings
}

func SetSettings(settings WorkerSettings) {
	workerSettings = settings
}

func SetRedisSettings(redisSettings RedisSettings) {
	workerSettings.RedisSettings = redisSettings
}

type RedisSettings struct {
	URI        string
	Host       string
	DB         string
	Scheme     string
	MasterName string
	Sentinels  []string
	Timeout    time.Duration
	Password   string
}

// Init initializes the goworker process. This will be
// called by the Work function, but may be used by programs
// that wish to access goworker functions and configuration
// without actually processing jobs.
func Init() error {
	initMutex.Lock()
	defer initMutex.Unlock()
	if !initialized {
		var err error
		logger, err = seelog.LoggerFromWriterWithMinLevel(os.Stdout, seelog.InfoLvl)
		if err != nil {
			return err
		}

		if err := flags(); err != nil {
			return err
		}
		ctx = context.Background()
		if workerSettings.URI != "" {
			workerSettings.RedisSettings.URI = workerSettings.URI
		}
		pool = newRedisPool(workerSettings.RedisSettings, workerSettings.Connections, workerSettings.Connections, time.Minute)

		initialized = true
	}
	return nil
}

// GetConn returns a connection from the goworker Redis
// connection pool. When using the pool, check in
// connections as quickly as possible, because holding a
// connection will cause concurrent worker functions to lock
// while they wait for an available connection. Expect this
// API to change drastically.
func GetConn() (*RedisConn, error) {
	var connectionAttempts int
	if isSentinelConnection() {
		connectionAttempts = workerSettings.Connections + 1
	} else {
		connectionAttempts = 1
	}
	return getConn(connectionAttempts)
}

func getConn(attemptsLeft int) (*RedisConn, error) {
	if attemptsLeft <= 0 {
		return nil, errors.New("Unable to get connection")
	}

	resource, err := pool.Get(ctx)
	if err != nil {
		// If we get a timeout when connection to the redis server
		// we should retry it
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			return getConn(attemptsLeft - 1)
		} else {
			return nil, err
		}
	}

	conn := resource.(*RedisConn)
	ok := validateConnection(conn)
	if !ok {
		// If our connection is not valid, we need to remove it from the pool and try again
		conn.Close()
		pool.Put(nil)
		return getConn(attemptsLeft - 1)
	}

	return conn, nil
}

// PutConn puts a connection back into the connection pool.
// Run this as soon as you finish using a connection that
// you got from GetConn. Expect this API to change
// drastically.
func PutConn(conn *RedisConn) {
	pool.Put(conn)
}

// Close cleans up resources initialized by goworker. This
// will be called by Work when cleaning up. However, if you
// are using the Init function to access goworker functions
// and configuration without processing jobs by calling
// Work, you should run this function when cleaning up. For
// example,
//
//	if err := goworker.Init(); err != nil {
//		fmt.Println("Error:", err)
//	}
//	defer goworker.Close()
func Close() {
	initMutex.Lock()
	defer initMutex.Unlock()
	if initialized {
		pool.Close()
		initialized = false
	}
}

// Work starts the goworker process. Check for errors in
// the return value. Work will take over the Go executable
// and will run until a QUIT, INT, or TERM signal is
// received, or until the queues are empty if the
// -exit-on-complete flag is set.
func Work() error {
	err := Init()
	if err != nil {
		return err
	}
	defer Close()

	quit := signals()

	poller, err := newPoller(workerSettings.Queues, workerSettings.IsStrict)
	if err != nil {
		return err
	}
	jobs := poller.poll(time.Duration(workerSettings.Interval), quit)

	var monitor sync.WaitGroup

	for id := 0; id < workerSettings.Concurrency; id++ {
		worker, err := newWorker(strconv.Itoa(id), workerSettings.Queues)
		if err != nil {
			return err
		}
		worker.work(jobs, &monitor)
	}

	monitor.Wait()

	return nil
}
